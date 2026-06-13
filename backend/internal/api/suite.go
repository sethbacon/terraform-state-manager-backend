package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

const suiteIssuer = "terraform-state-manager"

func buildSuiteManifest(cfg *config.Config) suite.Manifest {
	pub := cfg.Server.PublicURL
	if pub == "" {
		pub = cfg.Server.BaseURL
	}
	return suite.Manifest{
		SchemaVersion: suite.SchemaVersionV1,
		App:           "terraform-state-manager",
		Version:       AppVersion,
		BuildDate:     AppBuildDate,
		PublicURL:     pub,
		Identity:      suite.IdentityInfo{Issuer: suiteIssuer, SharedStore: false, Schema: "identity"},
		Capabilities:  []suite.Capability{{ID: "state.v1"}},
		Links:         map[string]string{"sourceDetail": "/sources/{id}"},
	}
}

func suiteManifestHandler(cfg *config.Config) gin.HandlerFunc {
	m := buildSuiteManifest(cfg)
	return func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=30")
		c.JSON(http.StatusOK, m)
	}
}

func uiConfigHandler(getClient func() *suite.DiscoveryClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := gin.H{"sibling": nil}
		if dc := getClient(); dc != nil {
			if state, m := dc.Snapshot(); state == suite.StateActive && m != nil {
				out["sibling"] = gin.H{
					"app": m.App, "state": string(state),
					"publicUrl": m.PublicURL, "links": m.Links,
				}
			} else {
				out["sibling"] = gin.H{"state": string(state)}
			}
		}
		c.JSON(http.StatusOK, out)
	}
}
