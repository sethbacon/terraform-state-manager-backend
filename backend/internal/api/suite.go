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
	// audit.ingest.v1 is advertised only under a shared identity store: it tells a
	// sibling it may federate its audit trail here (POST /audit/ingest), which is
	// only coherent when user/org IDs are shared. Standalone/federated-DB mode
	// omits it so a sibling never ships entries that would mis-attribute or fail
	// the user_id FK.
	caps := []suite.Capability{{ID: "state.v1"}}
	if cfg.Suite.IdentitySharedStore {
		caps = append(caps, suite.Capability{ID: "audit.ingest.v1"})
	}
	return suite.Manifest{
		SchemaVersion: suite.SchemaVersionV1,
		App:           "terraform-state-manager",
		Version:       AppVersion,
		BuildDate:     AppBuildDate,
		// UntrustedURL is the type a SIBLING's manifest field carries, because a
		// consumer must not concatenate a sibling-asserted URL into a request.
		// This is OUR OWN manifest, built from OUR OWN configuration, so the
		// conversion is the trusted direction: we are asserting it, not
		// consuming it.
		PublicURL:    suite.UntrustedURL(pub),
		Identity:     suite.IdentityInfo{Issuer: suiteIssuer, SharedStore: cfg.Suite.IdentitySharedStore, Schema: "identity"},
		Capabilities: caps,
		Links:        map[string]string{"sourceDetail": "/sources/{id}"},
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
					// Display, not Resolve: this payload is rendered by the SPA, never
					// fetched by this backend. Resolve is for the outbound path (see
					// ListStateModuleFreshness), and using it here would fail the whole
					// UI-config read for a sibling this app has no intention of dialing.
					"publicUrl": m.PublicURL.Display(), "links": m.Links,
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
