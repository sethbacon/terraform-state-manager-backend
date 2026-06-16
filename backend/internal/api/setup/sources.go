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
		enc, err := crypto.Encrypt(plain)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt credentials"})
			return
		}
		src.EncryptedCredentials = enc
	}
	if _, err := h.sources.Create(c.Request.Context(), src); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create state source"})
		return
	}
	if err := h.settings.SetSourcesConfigured(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record source status"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"status": "ok"})
}
