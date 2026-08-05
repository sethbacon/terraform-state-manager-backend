// roots.go confines the connectors whose configuration names a path on the
// SERVER's own filesystem — the local connector's base_path and the kubernetes
// connector's kubeconfig — to roots the operator explicitly permitted.
//
// Those paths arrive with the source record, i.e. from the API caller holding
// sources:manage, not from the operator who deployed the server. The invariant
// this file enforces is therefore: a server-local path named by a source must
// resolve inside a configured root. It is checked once, at connector
// construction, so every later list/read/write/delete/lock inherits it rather
// than each needing its own guard.
//
// Enforcement FAILS CLOSED. Both lists are empty by default, and an empty list
// means "no source may name a server-local path" — never "any path is fine".
// A deployment that serves state from a mounted directory sets
// statesource.local_roots (TSM_STATESOURCE_LOCAL_ROOTS) to exactly the mount
// point(s) it exposes; one that hands the kubernetes connector a kubeconfig
// file sets statesource.kubeconfig_roots (TSM_STATESOURCE_KUBECONFIG_ROOTS).
package statesource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// permittedLocalRoots and permittedKubeconfigRoots are process globals set once
// at startup from configuration (same lifecycle as egressGuard: written by the
// Configure* functions before any connector is constructed, read-only after).
var (
	permittedLocalRoots      []string
	permittedKubeconfigRoots []string
)

// ConfigureLocalRoots sets the directories a local source's base_path may name —
// a root itself, or anything beneath it. Entries must be absolute paths. Call
// once at startup, before any connector is constructed; an empty list leaves
// local sources unavailable (fail closed).
func ConfigureLocalRoots(roots []string) error {
	normalized, err := normalizeRoots(roots)
	if err != nil {
		return err
	}
	permittedLocalRoots = normalized
	return nil
}

// ConfigureKubeconfigRoots sets the directories (or exact files) a kubernetes
// source's config.kubeconfig may name. Same contract as ConfigureLocalRoots;
// empty means a kubernetes source must be configured with server + token
// instead of a file on the server.
func ConfigureKubeconfigRoots(roots []string) error {
	normalized, err := normalizeRoots(roots)
	if err != nil {
		return err
	}
	permittedKubeconfigRoots = normalized
	return nil
}

// normalizeRoots trims, de-duplicates and validates configured roots. Relative
// entries are rejected rather than resolved against the process working
// directory, which would make the boundary depend on how the server happened to
// be launched.
func normalizeRoots(roots []string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("permitted root %q must be an absolute path", r)
		}
		clean := filepath.Clean(r)
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out, nil
}

// ensureLocalBasePathPermitted enforces the local-source boundary on an absolute
// base_path. Callers apply it twice: before touching the path at all (so an
// unpermitted path is never probed) and again on its symlink-resolved form.
func ensureLocalBasePathPermitted(path string) error {
	if len(permittedLocalRoots) == 0 {
		return fmt.Errorf("local sources are unavailable: no permitted base_path roots are configured (statesource.local_roots / TSM_STATESOURCE_LOCAL_ROOTS)")
	}
	if !containedInRoots(path, permittedLocalRoots) {
		// Name the path AND the configured roots: failing closed only helps an
		// operator if the failure says what was rejected and what would be
		// accepted. Both values are operator-supplied configuration, not caller
		// secrets, so echoing them leaks nothing the operator does not own.
		return fmt.Errorf("base_path %q is outside the permitted local state-source roots %v (statesource.local_roots / TSM_STATESOURCE_LOCAL_ROOTS)",
			path, permittedLocalRoots)
	}
	return nil
}

// ensureKubeconfigPermitted is ensureLocalBasePathPermitted for the kubernetes
// connector's kubeconfig file.
func ensureKubeconfigPermitted(path string) error {
	if len(permittedKubeconfigRoots) == 0 {
		return fmt.Errorf("a kubeconfig path is unavailable: no permitted roots are configured (statesource.kubeconfig_roots / TSM_STATESOURCE_KUBECONFIG_ROOTS) — configure config.server + credentials.token instead")
	}
	if !containedInRoots(path, permittedKubeconfigRoots) {
		return fmt.Errorf("kubeconfig path %q is outside the permitted roots %v (statesource.kubeconfig_roots / TSM_STATESOURCE_KUBECONFIG_ROOTS)",
			path, permittedKubeconfigRoots)
	}
	return nil
}

// containedInRoots reports whether path (absolute and cleaned) sits inside at
// least one permitted root.
func containedInRoots(path string, roots []string) bool {
	for _, root := range roots {
		for _, form := range rootForms(root) {
			if pathWithin(path, form) {
				return true
			}
		}
	}
	return false
}

// rootForms returns the paths a candidate may legitimately match for one
// configured root: the root as written, plus its symlink-free real path when it
// resolves. Both are needed because a candidate is compared before AND after its
// own symlinks are resolved — a root that is itself a symlink (a mounted volume
// reached through a link) must match either way.
func rootForms(root string) []string {
	forms := []string{root}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		forms = append(forms, real)
	}
	return forms
}

// pathWithin reports whether path is root itself or lies beneath it. The
// comparison is on path SEGMENT boundaries, so a sibling whose name merely
// starts with the root's ("/data/states-evil" against a root of "/data/states")
// is not treated as contained.
func pathWithin(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}
