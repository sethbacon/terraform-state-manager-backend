// apikeys.go implements API-key self-service modeled on the registry: users
// create and manage their own keys (carrying only scopes they themselves
// hold), admins see everyone's; the secret is returned exactly once on
// create/rotate. Storage, hashing (bcrypt, 10-char lookup prefix), and the
// api_keys table come from the shared identity module; the middleware
// authenticates keys presented as Bearer tokens (see internal/middleware).
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	"github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

// maxRotationGraceHours bounds the overlap window during key rotation.
const maxRotationGraceHours = 72

// assignableKeyScopes are the scopes a key may carry. SCIM provisioning keeps
// its own token path and is deliberately not key-assignable. ScopeAdmin is
// also excluded (#252): an admin-scoped key is a durable bearer credential —
// it bypasses the cookie double-submit CSRF check, is not bound by the 24h
// session TTL, and may be minted with no expiry — so admin actions must go
// through the interactive session rather than a long-lived key.
var assignableKeyScopes = []auth.Scope{
	auth.ScopeStateRead, auth.ScopeStateWrite, auth.ScopeStateDrift,
	auth.ScopeStateExecute, auth.ScopeStateTransfer, auth.ScopeSourcesManage,
}

// APIKeysHandlers serves /api/v1/apikeys.
type APIKeysHandlers struct {
	keys  *idstore.APIKeyRepository
	orgs  *idstore.OrganizationRepository
	audit auditor
}

// NewAPIKeysHandlers constructs the handlers over the identity connection.
func NewAPIKeysHandlers(identityDB *sql.DB) *APIKeysHandlers {
	return &APIKeysHandlers{
		keys:  idstore.NewAPIKeyRepository(identityDB),
		orgs:  idstore.NewOrganizationRepository(identityDB),
		audit: newAuditor(identityDB),
	}
}

func scopesOf(c *gin.Context) []string {
	if v, ok := c.Get("scopes"); ok {
		if s, ok := v.([]string); ok {
			return s
		}
	}
	return []string{}
}

func isAdmin(c *gin.Context) bool {
	return auth.HasScope(scopesOf(c), auth.ScopeAdmin)
}

// validateGrantedScopes enforces the registry rule: a key may only carry
// scopes its creator holds, drawn from the assignable set.
func validateGrantedScopes(c *gin.Context, requested []string) (ok bool, bad string) {
	creator := scopesOf(c)
	for _, s := range requested {
		known := false
		for _, a := range assignableKeyScopes {
			if s == string(a) {
				known = true
				break
			}
		}
		if !known {
			return false, s
		}
		if !auth.HasScope(creator, auth.Scope(s)) {
			return false, s
		}
	}
	return true, ""
}

// keyScope is the tenant constraint every api_keys accessor in this file
// carries.
//
// It is OrgScopeAllOrganizations() by design, and that decision belongs here
// rather than at each call site. In terraform-registry an api_keys row's
// organization_id is a real authority binding, so scoping by it is the tenant
// boundary. In TSM it is not: mintKey stamps EVERY key with the GLOBAL DEFAULT
// organization, whoever the owner is and whatever they are a member of. A scope
// over ak.organization_id here would therefore mean "is the default
// organization in your scope" — which shows an admin of the default
// organization every other tenant's keys, and an admin of any other
// organization none of their own. The tenant boundary for a TSM key is its
// OWNER's membership, which is what ownsOrAdmin and keysVisibleToScope apply.
//
// Narrowing this properly is a schema change (bind a key to the organization
// whose authority it draws on) and is out of scope for the identity bump; it is
// reported rather than half-applied here.
func keyScope() idstore.OrgScope { return idstore.OrgScopeAllOrganizations() }

// ownsOrAdmin loads the key and authorizes the caller (owner or admin),
// answering 404/403/500 itself when access is denied.
func (h *APIKeysHandlers) ownsOrAdmin(c *gin.Context) (*models.APIKey, bool) {
	// Sentinel first: an unknown key id keeps answering 404. A bare `err != nil`
	// would 500 here since v0.24.0 and leave the 404 below unreachable — which
	// would also leak existence, since the "hide other users' keys" branch just
	// below deliberately answers 404 too. Since v0.25.0 a key OUTSIDE the scope
	// reports the same ErrNotFound, so refused access and absent row are
	// indistinguishable to the caller either way.
	key, err := h.keys.GetAPIKeyByID(c.Request.Context(), c.Param("id"), keyScope())
	if errors.Is(err, idstore.ErrNotFound) || (err == nil && key == nil) {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return nil, false
	}
	if err != nil {
		serverError(c, err, "failed to load API key")
		return nil, false
	}
	if !isAdmin(c) && (key.UserID == nil || *key.UserID != userIDOf(c)) {
		// Hide other users' keys entirely.
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return nil, false
	}
	return key, true
}

// ListAPIKeys returns the caller's keys; admins see every key.
// @Summary      List API keys
// @Tags         APIKeys
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /apikeys [get]
func (h *APIKeysHandlers) ListAPIKeys() gin.HandlerFunc {
	return func(c *gin.Context) {
		var (
			keys []*models.APIKey
			err  error
		)
		if isAdmin(c) {
			// A single-org admin's flat ScopeAdmin reaches this branch, so scope the
			// admin view to keys whose OWNER shares an organization the caller
			// administers (#182). Every key is tagged with the default org at mint
			// time (see mintKey), so the owner's org membership — not the key's own
			// organization_id — is the tenant boundary. See keyScope.
			adminOrgs, aErr := h.orgs.OrgScopeForUser(c.Request.Context(), userIDOf(c),
				string(auth.ScopeAdmin), auth.ReadWritePairs())
			if aErr != nil {
				serverError(c, aErr, "failed to resolve caller organizations")
				return
			}
			all, lErr := h.keys.ListAPIKeys(c.Request.Context(), keyScope())
			if lErr != nil {
				serverError(c, lErr, "failed to list API keys")
				return
			}
			keys, err = h.keysVisibleToScope(c.Request.Context(), all, adminOrgs.WithUnowned())
		} else {
			keys, err = h.keys.ListAPIKeysByUser(c.Request.Context(), userIDOf(c), keyScope())
		}
		if err != nil {
			serverError(c, err, "failed to list API keys")
			return
		}
		if keys == nil {
			keys = []*models.APIKey{}
		}
		c.JSON(http.StatusOK, gin.H{"keys": keys})
	}
}

// keysVisibleToScope keeps only keys whose OWNER falls inside scope.
//
// This is the ONE place in this repo that still narrows organization-owned rows
// after the database rather than in the query, and it is deliberate: the row's
// own organization_id is not the boundary here (see keyScope), the owner's
// membership is, and identity exposes no api_keys accessor that joins through
// organization_members. The membership test itself is not re-implemented — it
// is store.OrgScope.PermitsOrganization, the in-memory half of the same
// predicate the SQL builds, so this cannot drift from it. Owners with no
// membership are kept when the scope admits unowned rows, mirroring the
// user-list rule; each distinct owner is looked up once.
func (h *APIKeysHandlers) keysVisibleToScope(ctx context.Context, keys []*models.APIKey, scope idstore.OrgScope) ([]*models.APIKey, error) {
	allowed := map[string]bool{}
	out := []*models.APIKey{}
	for _, k := range keys {
		if k.UserID == nil {
			continue
		}
		owner := *k.UserID
		ok, seen := allowed[owner]
		if !seen {
			memberships, mErr := h.orgs.GetUserMemberships(ctx, owner)
			if mErr != nil {
				return nil, mErr
			}
			// An owner who belongs to nothing is the users-table "unowned" case.
			ok = len(memberships) == 0 && scope.IncludesUnowned()
			for _, m := range memberships {
				if scope.PermitsOrganization(m.OrganizationID) {
					ok = true
					break
				}
			}
			allowed[owner] = ok
		}
		if ok {
			out = append(out, k)
		}
	}
	return out, nil
}

type apiKeyInput struct {
	Name        string   `json:"name" binding:"required"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"` // RFC3339; empty = never
}

func parseExpiry(c *gin.Context, raw string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at must be RFC3339 (e.g. 2026-12-31T00:00:00Z)"})
		return nil, false
	}
	if t.Before(time.Now()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at is in the past"})
		return nil, false
	}
	return &t, true
}

// CreateAPIKey mints a key for the caller. The full secret appears ONLY in
// this response.
// @Summary      Create API key
// @Description  Creates an API key carrying a subset of the caller's scopes. The full key is returned once and never again.
// @Tags         APIKeys
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /apikeys [post]
func (h *APIKeysHandlers) CreateAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req apiKeyInput
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if len(req.Scopes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "at least one scope is required"})
			return
		}
		if ok, bad := validateGrantedScopes(c, req.Scopes); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot grant scope " + bad + " (keys may only carry scopes you hold)"})
			return
		}
		expires, ok := parseExpiry(c, req.ExpiresAt)
		if !ok {
			return
		}
		key, created, err := h.mintKey(c, req, expires, userIDOf(c))
		if err != nil {
			serverError(c, err, "failed to create API key")
			return
		}
		h.audit.write(c, "api_key.create", "api_key", created.ID,
			map[string]interface{}{"name": created.Name, "scopes": created.Scopes})
		c.JSON(http.StatusCreated, gin.H{"key": key, "api_key": created})
	}
}

// mintKey generates and persists a key owned by userID, returning the
// plaintext secret (shown once) and the stored row.
func (h *APIKeysHandlers) mintKey(c *gin.Context, req apiKeyInput, expires *time.Time, userID string) (string, *models.APIKey, error) {
	// Every key is stamped with the default organization, so a deployment
	// without one cannot mint keys at all. Before v0.24.0 that arrived as
	// (nil, nil) and this function dereferenced org.ID into a panic; naming the
	// sentinel turns it into an explicit, greppable 500.
	org, err := h.orgs.GetDefaultOrganization(c.Request.Context())
	if errors.Is(err, idstore.ErrNotFound) {
		return "", nil, fmt.Errorf("default organization not found: %w", err)
	}
	if err != nil {
		return "", nil, err
	}
	if org == nil {
		return "", nil, fmt.Errorf("default organization not found")
	}
	fullKey, hash, prefix, err := idauth.GenerateAPIKey(middleware.APIKeyPrefix)
	if err != nil {
		return "", nil, err
	}
	row := &models.APIKey{
		UserID:         &userID,
		OrganizationID: org.ID,
		Name:           req.Name,
		KeyHash:        hash,
		KeyPrefix:      prefix,
		Scopes:         req.Scopes,
		ExpiresAt:      expires,
	}
	if req.Description != "" {
		row.Description = &req.Description
	}
	if err := h.keys.CreateAPIKey(c.Request.Context(), row, keyScope()); err != nil {
		return "", nil, err
	}
	return fullKey, row, nil
}

// GetAPIKey returns one key (owner or admin). Never includes the secret.
func (h *APIKeysHandlers) GetAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := h.ownsOrAdmin(c)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, key)
	}
}

// UpdateAPIKey changes a key's name, description, scopes, or expiry (owner or
// admin; scope grants re-validated against the caller).
func (h *APIKeysHandlers) UpdateAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := h.ownsOrAdmin(c)
		if !ok {
			return
		}
		var req apiKeyInput
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if len(req.Scopes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "at least one scope is required"})
			return
		}
		if ok, bad := validateGrantedScopes(c, req.Scopes); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot grant scope " + bad + " (keys may only carry scopes you hold)"})
			return
		}
		expires, ok := parseExpiry(c, req.ExpiresAt)
		if !ok {
			return
		}
		key.Name = req.Name
		key.Description = nil
		if req.Description != "" {
			key.Description = &req.Description
		}
		key.Scopes = req.Scopes
		key.ExpiresAt = expires
		// ownsOrAdmin read the row a moment ago, so a zero-row UPDATE means it
		// was deleted in between: same 404 the read would have given.
		if err := h.keys.Update(c.Request.Context(), key, keyScope()); err != nil {
			if errors.Is(err, idstore.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
				return
			}
			serverError(c, err, "failed to update API key")
			return
		}
		h.audit.write(c, "api_key.update", "api_key", key.ID,
			map[string]interface{}{"name": key.Name, "scopes": key.Scopes})
		c.JSON(http.StatusOK, key)
	}
}

// DeleteAPIKey revokes a key immediately (owner or admin).
func (h *APIKeysHandlers) DeleteAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := h.ownsOrAdmin(c)
		if !ok {
			return
		}
		// Idempotent: a key already destroyed by a concurrent rotation or sweep
		// is the desired end state of a DELETE, so it keeps its 204 rather than
		// becoming the 500 a bare `err != nil` would now yield.
		if err := h.keys.RevokeAPIKey(c.Request.Context(), key.ID, keyScope()); err != nil &&
			!errors.Is(err, idstore.ErrNotFound) {
			serverError(c, err, "failed to delete API key")
			return
		}
		h.audit.write(c, "api_key.delete", "api_key", key.ID, map[string]interface{}{"name": key.Name})
		c.Status(http.StatusNoContent)
	}
}

// RotateAPIKey mints a replacement with the same name/scopes/owner. With
// grace_period_hours=0 the old key is revoked immediately; otherwise it stays
// valid for up to 72h so consumers can switch over. The new secret appears
// ONLY in this response.
// @Summary      Rotate API key
// @Description  Mints a replacement key; the old key is revoked immediately or kept alive for grace_period_hours (max 72). Requires ownership or admin.
// @Tags         APIKeys
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "API key ID"
// @Success      201  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /apikeys/{id}/rotate [post]
func (h *APIKeysHandlers) RotateAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			GracePeriodHours int `json:"grace_period_hours"`
		}
		_ = c.ShouldBindJSON(&req) // body optional; default immediate
		if req.GracePeriodHours < 0 || req.GracePeriodHours > maxRotationGraceHours {
			c.JSON(http.StatusBadRequest, gin.H{"error": "grace_period_hours must be between 0 and 72"})
			return
		}
		key, ok := h.ownsOrAdmin(c)
		if !ok {
			return
		}

		owner := userIDOf(c)
		if key.UserID != nil {
			owner = *key.UserID // rotation preserves the original owner
		}
		input := apiKeyInput{Name: key.Name, Scopes: key.Scopes}
		if key.Description != nil {
			input.Description = *key.Description
		}
		secret, created, err := h.mintKey(c, input, key.ExpiresAt, owner)
		if err != nil {
			serverError(c, err, "failed to rotate API key")
			return
		}

		// The replacement is already minted and its secret is in this response
		// only. Both arms therefore treat "the old key is already gone" as the
		// goal having been met — a 500 here would leave the caller believing the
		// rotation failed while holding the only copy of the new secret. A real
		// failure still 500s.
		if req.GracePeriodHours == 0 {
			if err := h.keys.RevokeAPIKey(c.Request.Context(), key.ID, keyScope()); err != nil &&
				!errors.Is(err, idstore.ErrNotFound) {
				serverError(c, err, "rotated, but failed to revoke the old key")
				return
			}
		} else {
			cutoff := time.Now().Add(time.Duration(req.GracePeriodHours) * time.Hour)
			key.ExpiresAt = &cutoff
			if err := h.keys.Update(c.Request.Context(), key, keyScope()); err != nil &&
				!errors.Is(err, idstore.ErrNotFound) {
				serverError(c, err, "rotated, but failed to schedule the old key's expiry")
				return
			}
		}
		h.audit.write(c, "api_key.rotate", "api_key", key.ID,
			map[string]interface{}{"name": key.Name, "replacement": created.ID, "grace_hours": req.GracePeriodHours})
		c.JSON(http.StatusCreated, gin.H{"key": secret, "api_key": created, "old_key_id": key.ID})
	}
}
