// module_freshness.go implements the cross-app module-freshness read: for each
// registry module captured in a state (state_module_refs), it asks the sibling
// registry what the latest published version is and reports whether the locked
// version in use is behind. This is a TSM-backend -> registry server call
// against the registry's PUBLIC versions endpoint — the REVERSE direction of the
// /consumers proxy, and unauthenticated (no suite service token). It is inert
// when standalone: with no active sibling every module is "no_registry" and the
// response is always HTTP 200, so the page never breaks when the registry is
// absent.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	semver "github.com/terraform-state-manager/terraform-state-manager/internal/version"
)

// freshnessTimeout bounds each per-module call to the sibling registry so a slow
// registry can't block the page (mirrors the /consumers proxy's 2s budget).
const freshnessTimeout = 2 * time.Second

// moduleFreshness is one module's freshness verdict. status is one of:
//   - up_to_date     the locked version is the latest published version
//   - behind         a newer non-deprecated version is published (latest set)
//   - constraint_only no locked version was captured (only a constraint) — nothing to compare
//   - no_registry    no active sibling registry, or the ref points at a different registry
//   - unknown        the registry was unreachable / returned nothing comparable
//
// current is the locked version in use (nil when constraint_only); latest is the
// newest non-deprecated version the sibling publishes (nil unless a comparison
// was made and a newer or equal version exists).
type moduleFreshness struct {
	ModuleSource string  `json:"module_source"`
	RegistryHost string  `json:"registry_host"`
	Current      *string `json:"current"`
	Latest       *string `json:"latest"`
	Status       string  `json:"status"`
}

// registryVersionsResponse is the subset of the sibling registry's
// GET /v1/modules/{namespace}/{name}/{system}/versions response we consume.
type registryVersionsResponse struct {
	Modules []struct {
		Versions []struct {
			Version    string `json:"version"`
			Deprecated bool   `json:"deprecated"`
		} `json:"versions"`
	} `json:"modules"`
}

// ListStateModuleFreshness reports, per captured module, whether the locked
// version in use is behind the latest version published by the sibling registry.
// @Summary      Module freshness vs the sibling registry
// @Tags         Sources
// @Produce      json
// @Param        id   path   string  true   "Source ID"
// @Param        key  query  string  false  "Restrict to a single state key"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/modules/freshness [get]
func (h *SourcesHandlers) ListStateModuleFreshness(getClient func() *suite.DiscoveryClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		refs, err := h.moduleRefRepo.ListBySource(c.Request.Context(), c.Param("id"), c.Query("key"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load module provenance"})
			return
		}

		// Resolve the active sibling registry, if any. Three stacked gates
		// (mirroring the registry's moduleConsumersHandler): a client must exist,
		// it must be StateActive with a public URL, and (per module, in
		// computeFreshness) the ref's host must match the sibling's host. Any miss
		// degrades that module to no_registry — the endpoint is always 200 and
		// never errors on a missing sibling.
		var siblingURL, siblingHost string
		if dc := getClient(); dc != nil {
			if state, m := dc.Snapshot(); state == suite.StateActive && m != nil && m.PublicURL != "" {
				siblingURL = strings.TrimRight(m.PublicURL, "/")
				siblingHost = suite.CanonicalHost(m.PublicURL)
			}
		}

		client := &http.Client{Timeout: freshnessTimeout}
		c.JSON(http.StatusOK, gin.H{"modules": computeFreshness(c.Request.Context(), client, siblingURL, siblingHost, refs)})
	}
}

// computeFreshness is the pure core: given the resolved sibling (siblingURL +
// canonical siblingHost; both empty when standalone) and the captured refs, it
// produces a freshness verdict per module. It is the part worth testing — the
// handler above only does the gin + DiscoveryClient plumbing.
func computeFreshness(ctx context.Context, client *http.Client, siblingURL, siblingHost string, refs []repositories.StateModuleRef) []moduleFreshness {
	latestCache := map[string]*string{} // module_source -> latest (fetch once per request)
	out := make([]moduleFreshness, 0, len(refs))
	for _, ref := range refs {
		mf := moduleFreshness{
			ModuleSource: ref.ModuleSource,
			RegistryHost: ref.RegistryHost,
			Current:      ref.ModuleVersion, // the locked version in use (nil = constraint only)
			Status:       "no_registry",
		}
		switch {
		case siblingURL == "" || siblingHost == "" || suite.CanonicalHost(ref.RegistryHost) != siblingHost:
			// No active sibling, or this module lives in a registry the active
			// sibling can't answer for (e.g. public registry.terraform.io).
			mf.Status = "no_registry"
		case ref.ModuleVersion == nil:
			// Only a version constraint was captured (no lockfile ingested).
			mf.Status = "constraint_only"
		default:
			latest, cached := latestCache[ref.ModuleSource]
			if !cached {
				latest = latestRegistryVersion(ctx, client, siblingURL, ref.ModuleSource)
				latestCache[ref.ModuleSource] = latest
			}
			switch {
			case latest == nil:
				mf.Status = "unknown" // registry unreachable / no comparable versions
			case semver.Compare(*ref.ModuleVersion, *latest) >= 0:
				mf.Latest = latest
				mf.Status = "up_to_date"
			default:
				mf.Latest = latest
				mf.Status = "behind"
			}
		}
		out = append(out, mf)
	}
	return out
}

// latestRegistryVersion fetches the sibling registry's versions for moduleSource
// (a "namespace/name/system" address) and returns the highest non-deprecated
// semantic version, or nil if the registry is unreachable, non-200, returns no
// module, or has no comparable version. It never returns an error — a nil result
// maps to the "unknown" status so one bad module can't fail the whole response.
func latestRegistryVersion(ctx context.Context, client *http.Client, siblingURL, moduleSource string) *string {
	target := siblingURL + "/v1/modules/" + moduleSource + "/versions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body registryVersionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	latest := ""
	for _, m := range body.Modules {
		for _, v := range m.Versions {
			if v.Deprecated || !semver.IsValid(v.Version) {
				continue
			}
			if latest == "" || semver.Compare(v.Version, latest) > 0 {
				latest = v.Version
			}
		}
	}
	if latest == "" {
		return nil
	}
	return &latest
}
