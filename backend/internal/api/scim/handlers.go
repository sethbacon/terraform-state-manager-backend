// Package scim implements SCIM 2.0 provisioning endpoints (RFC 7644) for user and
// group management by an external identity provider. It is a near-direct port of
// the registry's SCIM handlers, retargeted onto the shared identity store.
//
// SECURITY: every endpoint is bearer-token authenticated and gated by the
// scim:provision scope (admin satisfies it); the route group is mounted only when
// auth.scim.enabled is set, so the surface does not exist by default. The endpoints
// take no cookies, so they are not CSRF-eligible. As with the other password/token
// endpoints, request-rate limiting belongs at the proxy/gateway in front of the API.
// Deletes/deactivations are soft (membership removal), never hard-deletes.
package scim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
)

// SCIM schema URIs.
const (
	SchemaUser     = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup    = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaListResp = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaError    = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaPatchOp  = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

// Handlers provides the SCIM 2.0 REST endpoints, backed by the shared identity store.
type Handlers struct {
	cfg      *config.Config
	userRepo *idstore.UserRepository
	orgRepo  *idstore.OrganizationRepository
	// creds invalidates the credential families that snapshot a deprovisioned
	// user's authority.
	//
	// SCIM is the primary IdP-driven offboarding channel: it is what fires when
	// HR disables an account. Every deactivation path here used to strip
	// organization memberships and nothing else (#330) — the deactivated user
	// kept a working session for the remainder of the 24h JWT lifetime and kept
	// their API-key rows permanently, because both families snapshot their
	// authority at issue time.
	//
	// May be nil (no sweep) so the handler set stays constructible without the
	// revocation subsystem.
	creds *credlifecycle.Sweeper
}

// Option configures optional Handlers construction behaviour.
type Option func(*Handlers)

// WithCredentialSweeper wires the credential sweep every deprovisioning path
// performs (DELETE /Users/{id}, and active=false via PUT or PATCH).
func WithCredentialSweeper(s *credlifecycle.Sweeper) Option {
	return func(h *Handlers) { h.creds = s }
}

// NewHandlers creates a SCIM handler set. identityDB resolves to the identity
// schema (search_path), like the other identity-backed handlers.
func NewHandlers(cfg *config.Config, identityDB *sql.DB, opts ...Option) *Handlers {
	h := &Handlers{
		cfg:      cfg,
		userRepo: idstore.NewUserRepository(identityDB),
		orgRepo:  idstore.NewOrganizationRepository(identityDB),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// deprovisionUser is the ONE way this package deactivates a principal: it
// removes every organization membership AND invalidates every credential family
// that carries a snapshot of the authority those memberships granted.
//
// The pairing is structural on purpose (#330). Four endpoints deactivate a user
// — DELETE /Users/:id, PUT with active=false, and both PATCH "replace" forms —
// and each of them previously called RemoveAllMembershipsForUser directly. A
// convention that every one of those call sites must also remember to sweep is
// the weakest possible guard: it held at three sites and was forgotten at the
// fourth, which is exactly how this class regresses. Removing the memberships
// without sweeping is now unspellable in this package.
//
// The sweep is best-effort and never fails the request: the membership strip has
// already committed, and SCIM clients retry aggressively on 5xx, so a sweep
// failure is logged rather than turned into an error the IdP would replay.
//
// TENANCY. The strip runs at OrgScopeAllOrganizations(), and that is a decision,
// not an omission. identity #162 narrows this call so a scim:provision holder
// cannot strip memberships in organizations they have no relationship with. TSM
// cannot take that narrowing without breaking what this endpoint is FOR: there
// is one SCIM surface for the whole deployment, driven by one directory, and a
// deactivation is the directory asserting something about the WHOLE principal.
// Scoped to the caller, an HR offboarding would strip the IdP service account's
// own organizations and leave the departing employee a live member — with a
// live JWT scope union — everywhere else, which is a strictly worse outcome than
// the one the narrowing prevents. The mitigations that make this defensible are
// that scim:provision is not key-assignable (see api.assignableKeyScopes) and
// that no seeded role template carries it, so it is reachable only through the
// admin wildcard. Recorded in admin_audit_scope_test.go's reviewed list.
//
// The returned scope names the organizations actually emptied, which the old
// int64 count could not, so the audit trail says WHERE authority was withdrawn.
func (h *Handlers) deprovisionUser(ctx context.Context, userID, reason string) error {
	// A bulk sweep reports what it removed, not ErrNotFound: a user with no
	// memberships left to strip is an already-deprovisioned user, which is the
	// desired end state and must not fail an IdP-driven deactivation that the
	// client will replay.
	removed, err := h.orgRepo.RemoveAllMembershipsForUser(ctx, userID, idstore.OrgScopeAllOrganizations())
	if err != nil {
		return err
	}
	out := h.creds.UserDeprovisioned(ctx, userID, reason)
	slog.Info("scim: credentials revoked for deprovisioned user",
		"id", userID, "reason", reason,
		"organizations_emptied", removed.OrganizationIDs(),
		"tokens_revoked", out.TokensRevoked, "api_keys_revoked", out.KeysRevoked,
		"incomplete", out.Incomplete)
	return nil
}

// directoryScope is the tenancy every SCIM accessor here carries: the whole
// directory. SCIM is a machine-to-machine sync with an external IdP that is
// authoritative for the entire deployment's user and group population — a
// partial view would make it reconcile toward deleting the users and groups it
// could not see. See deprovisionUser for the full argument and the mitigations.
func directoryScope() idstore.OrgScope { return idstore.OrgScopeAllOrganizations() }

// --- SCIM resource types ---

// SCIMUser is a SCIM 2.0 User resource representation.
type SCIMUser struct {
	Schemas    []string    `json:"schemas"`
	ID         string      `json:"id"`
	ExternalID string      `json:"externalId,omitempty"`
	UserName   string      `json:"userName"`
	Name       *SCIMName   `json:"name,omitempty"`
	Emails     []SCIMEmail `json:"emails,omitempty"`
	// Active is a POINTER so an omitted "active" is distinguishable from an
	// explicit "active": false.
	//
	// As a plain bool it zero-valued to false on every PUT that did not mention
	// the attribute, and PutUser reads false as "deprovision" — which since #330
	// means removing every organization membership AND irreversibly revoking
	// every API key the user holds, plus every live session. A partial PUT from
	// an IdP (or any client updating only a display name) would silently destroy
	// the user's credentials. Deprovisioning must require the IdP to actually say
	// active=false.
	Active *bool    `json:"active,omitempty"`
	Meta   SCIMMeta `json:"meta"`
}

// SCIMName is the SCIM name sub-object.
type SCIMName struct {
	Formatted  string `json:"formatted,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
}

// SCIMEmail is the SCIM email sub-object.
type SCIMEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary"`
}

// SCIMMeta is the SCIM metadata sub-object.
type SCIMMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	Location     string `json:"location,omitempty"`
}

// SCIMListResponse is the SCIM 2.0 ListResponse.
type SCIMListResponse struct {
	Schemas      []string    `json:"schemas"`
	TotalResults int         `json:"totalResults"`
	ItemsPerPage int         `json:"itemsPerPage"`
	StartIndex   int         `json:"startIndex"`
	Resources    interface{} `json:"Resources"`
}

// SCIMError is the SCIM 2.0 error response.
type SCIMError struct {
	Schemas  []string `json:"schemas"`
	Detail   string   `json:"detail"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
}

// SCIMPatchOp represents a SCIM PATCH request.
type SCIMPatchOp struct {
	Schemas    []string        `json:"schemas"`
	Operations []SCIMOperation `json:"Operations"`
}

// SCIMOperation is a single SCIM PATCH operation.
type SCIMOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value,omitempty"`
}

// --- User endpoints ---

// ListUsers handles GET /scim/v2/Users.
// @Summary      List SCIM users
// @Description  Returns a paginated list of users in SCIM 2.0 format. Supports an optional filter (e.g. userName eq "alice@example.com").
// @Tags         SCIM
// @Security     BearerAuth
// @Produce      json
// @Param        startIndex  query  int     false  "1-based start index"  default(1)
// @Param        count       query  int     false  "Page size (max 200)"  default(100)
// @Param        filter      query  string  false  "SCIM filter expression"
// @Success      200  {object}  scim.SCIMListResponse
// @Failure      401  {object}  scim.SCIMError
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Users [get]
func (h *Handlers) ListUsers() gin.HandlerFunc {
	return func(c *gin.Context) {
		startIndex, _ := strconv.Atoi(c.DefaultQuery("startIndex", "1"))
		count, _ := strconv.Atoi(c.DefaultQuery("count", "100"))
		if startIndex < 1 {
			startIndex = 1
		}
		if count < 1 || count > 200 {
			count = 100
		}
		filter := c.Query("filter")
		offset := startIndex - 1
		ctx := c.Request.Context()

		var users []*models.User
		var total int
		var err error

		if value := extractFilterValue(filter); filter != "" && value != "" {
			users, err = h.userRepo.Search(ctx, value, count, offset, directoryScope())
			total = len(users)
		} else {
			users, total, err = h.userRepo.ListUsers(ctx, count, offset, directoryScope())
		}
		if err != nil {
			slog.Error("scim: list users failed", "error", err)
			scimError(c, http.StatusInternalServerError, "Failed to list users")
			return
		}

		base := h.baseURL(c)
		resources := make([]SCIMUser, 0, len(users))
		for _, u := range users {
			resources = append(resources, userToSCIM(u, base))
		}

		c.JSON(http.StatusOK, SCIMListResponse{
			Schemas:      []string{SchemaListResp},
			TotalResults: total,
			ItemsPerPage: count,
			StartIndex:   startIndex,
			Resources:    resources,
		})
	}
}

// GetUser handles GET /scim/v2/Users/:id.
// @Summary      Get SCIM user
// @Description  Returns a single user in SCIM 2.0 format by ID.
// @Tags         SCIM
// @Security     BearerAuth
// @Produce      json
// @Param        id  path  string  true  "User ID"
// @Success      200  {object}  scim.SCIMUser
// @Failure      404  {object}  scim.SCIMError
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Users/{id} [get]
func (h *Handlers) GetUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")
		// Sentinel first: an unknown id keeps answering 404. A bare `err != nil`
		// would 500 every miss since v0.24.0, and SCIM clients retry 5xx
		// aggressively — a provisioning loop would hammer this route forever
		// over a user that simply does not exist.
		user, err := h.userRepo.GetUserByID(c.Request.Context(), userID, directoryScope())
		if errors.Is(err, idstore.ErrNotFound) || (err == nil && user == nil) {
			scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
			return
		}
		if err != nil {
			slog.Error("scim: get user failed", "id", userID, "error", err)
			scimError(c, http.StatusInternalServerError, "Failed to get user")
			return
		}
		c.JSON(http.StatusOK, userToSCIM(user, h.baseURL(c)))
	}
}

// CreateUser handles POST /scim/v2/Users.
// @Summary      Create SCIM user
// @Description  Provisions a new user via SCIM 2.0. Requires userName or emails[0].value; uses externalId as the identity link.
// @Tags         SCIM
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body  body  scim.SCIMUser  true  "SCIM user resource"
// @Success      201  {object}  scim.SCIMUser
// @Failure      400  {object}  scim.SCIMError
// @Failure      409  {object}  scim.SCIMError
// @Router       /scim/v2/Users [post]
func (h *Handlers) CreateUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req SCIMUser
		if err := c.ShouldBindJSON(&req); err != nil {
			scimError(c, http.StatusBadRequest, "Invalid SCIM user payload")
			return
		}

		email := req.UserName
		if email == "" && len(req.Emails) > 0 {
			email = req.Emails[0].Value
		}
		if email == "" {
			scimError(c, http.StatusBadRequest, "userName or emails[0].value is required")
			return
		}

		displayName := ""
		if req.Name != nil {
			displayName = req.Name.Formatted
			if displayName == "" {
				displayName = strings.TrimSpace(strings.Join([]string{req.Name.GivenName, req.Name.FamilyName}, " "))
			}
		}

		// Use externalId as the OIDC sub for SCIM-provisioned users (namespaced).
		oidcSub := "scim:" + req.ExternalID
		if req.ExternalID == "" {
			oidcSub = "scim:" + uuid.New().String()
		}

		// emailVerified=true: SCIM has no OIDC email_verified claim to check, but
		// SCIM provisioning already trusts the calling client out of band — it is
		// itself an authenticated, admin-configured directory-sync integration,
		// so the email it provisions is treated as coming from a trusted
		// directory (the same basis LDAP/SAML logins use here), not a
		// self-asserted value.
		ctx := c.Request.Context()
		user, err := h.userRepo.GetOrCreateUserFromOIDC(ctx, oidcSub, email, displayName, true)
		if err != nil {
			slog.Error("scim: create user failed", "email", email, "error", err)
			scimError(c, http.StatusConflict, "User already exists or creation failed")
			return
		}

		c.JSON(http.StatusCreated, userToSCIM(user, h.baseURL(c)))
	}
}

// PatchUser handles PATCH /scim/v2/Users/:id.
// @Summary      Patch SCIM user
// @Description  Partially updates a user via SCIM 2.0 PATCH operations. Supports the 'replace' op for active, userName, name.formatted, and displayName.
// @Tags         SCIM
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string            true  "User ID"
// @Param        body  body  scim.SCIMPatchOp  true  "SCIM PATCH request"
// @Success      200  {object}  scim.SCIMUser
// @Failure      400  {object}  scim.SCIMError
// @Failure      404  {object}  scim.SCIMError
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Users/{id} [patch]
func (h *Handlers) PatchUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")
		ctx := c.Request.Context()

		var patchReq SCIMPatchOp
		if err := c.ShouldBindJSON(&patchReq); err != nil {
			scimError(c, http.StatusBadRequest, "Invalid SCIM PATCH payload")
			return
		}

		user, err := h.userRepo.GetUserByID(ctx, userID, directoryScope())
		if err != nil || user == nil {
			scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
			return
		}

		for _, op := range patchReq.Operations {
			if strings.EqualFold(op.Op, "replace") {
				h.applyReplaceOp(ctx, user, op)
			}
			// Unsupported ops are ignored per the SCIM spec.
		}

		// Raced delete between the read above and this write: 404, matching the
		// read's own answer, so a SCIM client stops rather than retrying a 5xx.
		if err := h.userRepo.UpdateUser(ctx, user, directoryScope()); err != nil {
			if errors.Is(err, idstore.ErrNotFound) {
				scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
				return
			}
			slog.Error("scim: update user failed", "id", userID, "error", err)
			scimError(c, http.StatusInternalServerError, "Failed to update user")
			return
		}

		c.JSON(http.StatusOK, userToSCIM(user, h.baseURL(c)))
	}
}

// PutUser handles PUT /scim/v2/Users/:id (full replacement).
// @Summary      Replace SCIM user
// @Description  Full replacement of a user via SCIM 2.0 PUT. Setting active=false deactivates the user and removes all organization memberships.
// @Tags         SCIM
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string         true  "User ID"
// @Param        body  body  scim.SCIMUser  true  "Full SCIM user resource"
// @Success      200  {object}  scim.SCIMUser
// @Failure      400  {object}  scim.SCIMError
// @Failure      404  {object}  scim.SCIMError
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Users/{id} [put]
func (h *Handlers) PutUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")
		ctx := c.Request.Context()

		var req SCIMUser
		if err := c.ShouldBindJSON(&req); err != nil {
			scimError(c, http.StatusBadRequest, "Invalid SCIM user payload")
			return
		}

		user, err := h.userRepo.GetUserByID(ctx, userID, directoryScope())
		if err != nil || user == nil {
			scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
			return
		}

		if req.UserName != "" {
			user.Email = req.UserName
		}
		if req.Name != nil {
			displayName := req.Name.Formatted
			if displayName == "" {
				displayName = strings.TrimSpace(strings.Join([]string{req.Name.GivenName, req.Name.FamilyName}, " "))
			}
			if displayName != "" {
				user.Name = displayName
			}
		}

		// Only an EXPLICIT active=false deprovisions. An omitted attribute leaves
		// the user's authority — and their credentials — untouched.
		if req.Active != nil && !*req.Active {
			_ = h.deprovisionUser(ctx, userID, "scim: user deactivated via PUT")
			slog.Info("scim: user deactivated via PUT", "id", userID)
		}

		// Raced delete between the read above and this write: 404, matching the
		// read's own answer, so a SCIM client stops rather than retrying a 5xx.
		if err := h.userRepo.UpdateUser(ctx, user, directoryScope()); err != nil {
			if errors.Is(err, idstore.ErrNotFound) {
				scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
				return
			}
			slog.Error("scim: put user failed", "id", userID, "error", err)
			scimError(c, http.StatusInternalServerError, "Failed to update user")
			return
		}

		c.JSON(http.StatusOK, userToSCIM(user, h.baseURL(c)))
	}
}

// DeleteUser handles DELETE /scim/v2/Users/:id. It soft-deletes (deactivates) by
// removing all organization memberships; the user record is preserved.
// @Summary      Delete SCIM user
// @Description  Soft-deletes (deactivates) a user by removing all organization memberships. The user record is preserved.
// @Tags         SCIM
// @Security     BearerAuth
// @Param        id  path  string  true  "User ID"
// @Success      204  "User deactivated"
// @Failure      404  {object}  scim.SCIMError
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Users/{id} [delete]
func (h *Handlers) DeleteUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")
		ctx := c.Request.Context()

		user, err := h.userRepo.GetUserByID(ctx, userID, directoryScope())
		if err != nil || user == nil {
			scimError(c, http.StatusNotFound, fmt.Sprintf("User %q not found", userID))
			return
		}

		if err := h.deprovisionUser(ctx, userID, "scim: user deleted"); err != nil {
			slog.Error("scim: deactivate user failed", "id", userID, "error", err)
			scimError(c, http.StatusInternalServerError, "Failed to deactivate user")
			return
		}

		slog.Info("scim: user deactivated", "id", userID, "email", user.Email)
		c.Status(http.StatusNoContent)
	}
}

// --- Group endpoints (organizations mapped to SCIM groups) ---

// ListGroups handles GET /scim/v2/Groups.
// @Summary      List SCIM groups
// @Description  Returns all organizations as SCIM 2.0 Group resources (up to 200).
// @Tags         SCIM
// @Security     BearerAuth
// @Produce      json
// @Success      200  {object}  scim.SCIMListResponse
// @Failure      500  {object}  scim.SCIMError
// @Router       /scim/v2/Groups [get]
func (h *Handlers) ListGroups() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgs, err := h.orgRepo.List(c.Request.Context(), 200, 0, directoryScope())
		if err != nil {
			scimError(c, http.StatusInternalServerError, "Failed to list groups")
			return
		}

		base := h.baseURL(c)
		resources := make([]gin.H, 0, len(orgs))
		for _, org := range orgs {
			resources = append(resources, orgToSCIMGroup(org, base))
		}

		c.JSON(http.StatusOK, SCIMListResponse{
			Schemas:      []string{SchemaListResp},
			TotalResults: len(resources),
			ItemsPerPage: int(math.Min(float64(len(resources)), 200)),
			StartIndex:   1,
			Resources:    resources,
		})
	}
}

// GetGroup handles GET /scim/v2/Groups/:id.
// @Summary      Get SCIM group
// @Description  Returns a single organization as a SCIM 2.0 Group resource by ID.
// @Tags         SCIM
// @Security     BearerAuth
// @Produce      json
// @Param        id  path  string  true  "Group (organization) ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  scim.SCIMError
// @Router       /scim/v2/Groups/{id} [get]
func (h *Handlers) GetGroup() gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		org, err := h.orgRepo.GetByID(c.Request.Context(), groupID, directoryScope())
		if err != nil || org == nil {
			scimError(c, http.StatusNotFound, fmt.Sprintf("Group %q not found", groupID))
			return
		}
		c.JSON(http.StatusOK, orgToSCIMGroup(org, h.baseURL(c)))
	}
}

// --- Helpers ---

func (h *Handlers) applyReplaceOp(ctx context.Context, user *models.User, op SCIMOperation) {
	switch strings.ToLower(op.Path) {
	case "active":
		active := true
		switch v := op.Value.(type) {
		case bool:
			active = v
		case string:
			active = strings.EqualFold(v, "true")
		}
		if !active {
			_ = h.deprovisionUser(ctx, user.ID, "scim: user deactivated via PATCH")
			slog.Info("scim: user deactivated via PATCH", "id", user.ID)
		}
	case "username", "emails[type eq \"work\"].value":
		if v, ok := op.Value.(string); ok && v != "" {
			user.Email = v
		}
	case "name.formatted", "displayname":
		if v, ok := op.Value.(string); ok && v != "" {
			user.Name = v
		}
	case "":
		// No path — the value is a map of attributes.
		if m, ok := op.Value.(map[string]interface{}); ok {
			if v, ok := m["active"].(bool); ok && !v {
				_ = h.deprovisionUser(ctx, user.ID, "scim: user deactivated via pathless PATCH")
			}
			if v, ok := m["userName"].(string); ok && v != "" {
				user.Email = v
			}
			if nameMap, ok := m["name"].(map[string]interface{}); ok {
				if formatted, ok := nameMap["formatted"].(string); ok && formatted != "" {
					user.Name = formatted
				}
			}
		}
	}
}

func (h *Handlers) baseURL(c *gin.Context) string {
	if h.cfg.Server.PublicURL != "" {
		return strings.TrimRight(h.cfg.Server.PublicURL, "/")
	}
	scheme := "https"
	if c.Request.TLS == nil {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Request.Host)
}

func userToSCIM(u *models.User, baseURL string) SCIMUser {
	externalID := ""
	if u.OIDCSub != nil {
		externalID = strings.TrimPrefix(*u.OIDCSub, "scim:")
	}

	emails := []SCIMEmail{}
	if u.Email != "" {
		emails = append(emails, SCIMEmail{Value: u.Email, Type: "work", Primary: true})
	}

	active := true
	return SCIMUser{
		Schemas:    []string{SchemaUser},
		ID:         u.ID,
		ExternalID: externalID,
		UserName:   u.Email,
		Name:       &SCIMName{Formatted: u.Name},
		Emails:     emails,
		// Active is always true for existing users; deactivated users have no memberships.
		Active: &active,
		Meta: SCIMMeta{
			ResourceType: "User",
			Created:      u.CreatedAt.Format(time.RFC3339),
			LastModified: u.UpdatedAt.Format(time.RFC3339),
			Location:     fmt.Sprintf("%s/scim/v2/Users/%s", baseURL, u.ID),
		},
	}
}

func orgToSCIMGroup(org *models.Organization, baseURL string) gin.H {
	return gin.H{
		"schemas":     []string{SchemaGroup},
		"id":          org.ID,
		"displayName": org.Name,
		"meta": SCIMMeta{
			ResourceType: "Group",
			Created:      org.CreatedAt.Format(time.RFC3339),
			LastModified: org.UpdatedAt.Format(time.RFC3339),
			Location:     fmt.Sprintf("%s/scim/v2/Groups/%s", baseURL, org.ID),
		},
	}
}

// extractFilterValue parses the value from a simple SCIM `attr eq "value"` filter.
func extractFilterValue(filter string) string {
	parts := strings.SplitN(filter, " eq ", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(parts[1]), "\"")
}

func scimError(c *gin.Context, status int, detail string) {
	c.JSON(status, SCIMError{
		Schemas: []string{SchemaError},
		Detail:  detail,
		Status:  strconv.Itoa(status),
	})
}
