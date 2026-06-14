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
		Identity:      suite.IdentityInfo{Issuer: suiteIssuer, SharedStore: cfg.Suite.IdentitySharedStore, Schema: "identity"},
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

func uiConfigHandler(cfg *config.Config, getClient func() *suite.DiscoveryClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := gin.H{"sibling": nil}
		if dc := getClient(); dc != nil {
			if state, m := dc.Snapshot(); state == suite.StateActive && m != nil {
				// Single sign-on is seamless only when BOTH apps assert the shared
				// identity store; otherwise the SPA keeps its "you may need to sign
				// in" hint. issuer is informational (which app minted the sibling's
				// tokens). Forwarded only on the active branch so a stale identity
				// block can't leak during degraded/unreachable windows.
				out["sibling"] = gin.H{
					"app": m.App, "state": string(state),
					"publicUrl": m.PublicURL, "links": m.Links,
					"issuer":      m.Identity.Issuer,
					"sharedStore": cfg.Suite.IdentitySharedStore && m.Identity.SharedStore,
				}
			} else {
				out["sibling"] = gin.H{"state": string(state)}
			}
		}
		c.JSON(http.StatusOK, out)
	}
}
