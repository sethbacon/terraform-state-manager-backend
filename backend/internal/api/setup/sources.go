package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

const sourceTestTimeout = 15 * time.Second

type sourceRequest struct {
	Name        string         `json:"name" binding:"required"`
	Type        string         `json:"type" binding:"required"`
	Endpoint    string         `json:"endpoint"`
	Config      map[string]any `json:"config"`
	Scope       map[string]any `json:"scope"`
	Credentials map[string]any `json:"credentials"`
}

// TestSource validates connectivity to a candidate state source without saving:
// it builds the connector and lists its states.
func (h *Handlers) TestSource(c *gin.Context) {
	var req sourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "failed", "error": "name and type are required"})
		return
	}
	conn, err := statesource.New(req.Type, req.Config, req.Credentials)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "failed", "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sourceTestTimeout)
	defer cancel()
	refs, err := conn.List(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"status": "failed", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "states": len(refs)})
}

// SaveSource adds the first state source (credentials encrypted at rest) and
// records that sources are configured. Mirrors the runtime CreateSource path.
func (h *Handlers) SaveSource(c *gin.Context) {
	var req sourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and type are required"})
		return
	}
	// Validate the connector config up front (e.g. local base_path exists).
	if _, err := statesource.New(req.Type, req.Config, req.Credentials); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src := &repositories.Source{
		Name:     req.Name,
		Type:     req.Type,
		Endpoint: req.Endpoint,
		Config:   req.Config,
		Scope:    req.Scope,
	}
	if len(req.Credentials) > 0 {
		if !crypto.Available() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store credentials: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
			return
		}
		plain, _ := json.Marshal(req.Credentials)
		enc, err := crypto.EncryptFor(plain, crypto.PurposeStateSourceCredentials)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt credentials"})
			return
		}
		src.EncryptedCredentials = enc
	}
	// THE SETUP WIZARD HAS NO PRINCIPAL, so it has no acting organization to
	// read (#436). SetupTokenMiddleware is the only gate on this route: there is
	// no session, no API key, no user_id and no membership, so
	// tenantscope.ActingOrganization is inapplicable here rather than merely
	// awkward — it would return ErrNoActingOrganization every time.
	//
	// The source the wizard creates belongs to the deployment's DEFAULT
	// organization, read from the app-side carrier. That is the same
	// organization the wizard's owner step writes the first membership into, so
	// the first source and the first administrator land together by
	// construction rather than by coincidence.
	orgID, err := h.settings.DefaultOrganizationID(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve the default organization"})
		return
	}
	if orgID == "" {
		// Refuse rather than fall through to the column DEFAULT. bootstrap.Run
		// writes this carrier before the listener starts and main is fatal if it
		// errors, so an empty value here means the deployment did not boot the
		// way it must have — creating an unowned source on top of that would
		// hide it.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "no default organization is configured"})
		return
	}
	if _, err := h.sources.Create(c.Request.Context(), src, orgID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create state source"})
		return
	}
	if err := h.settings.SetSourcesConfigured(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record source status"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"status": "ok"})
}
