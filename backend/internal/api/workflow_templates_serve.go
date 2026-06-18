package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// builtinWorkflow returns the embedded built-in content for a (provider, kind)
// pair — the fallback served when no operator override exists in the store.
func builtinWorkflow(provider, kind string) string {
	switch kind {
	case "versionlab":
		if provider == "azure_devops" {
			return azureHealthPipeline
		}
		return githubHealthWorkflow
	default: // "drift"
		if provider == "azure_devops" {
			return azureDriftPipeline
		}
		return githubDriftWorkflow
	}
}

// serveWorkflowTemplate resolves (provider, kind, profile) against the
// operator-managed store first, falling back to the embedded built-in when no
// row exists. profile defaults to "default"; an unknown profile (or a nil repo,
// as in unit tests with no DB) transparently falls back, so existing callers
// keep working unchanged.
func serveWorkflowTemplate(templates *repositories.WorkflowTemplateRepository, kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := c.DefaultQuery("provider", "github_actions")
		profile := c.DefaultQuery("profile", "default")
		if templates != nil {
			t, err := templates.GetByKey(c.Request.Context(), provider, kind, profile)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load template"})
				return
			}
			if t != nil {
				c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(t.Content))
				return
			}
		}
		c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(builtinWorkflow(provider, kind)))
	}
}
