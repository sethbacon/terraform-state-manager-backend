package statesource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setLocalRoots installs the permitted local roots for one test and restores the
// package default afterwards (the lists are process globals, like the egress
// guard). Called with no arguments it configures the fail-closed empty list.
func setLocalRoots(t *testing.T, roots ...string) {
	t.Helper()
	prev := permittedLocalRoots
	t.Cleanup(func() { permittedLocalRoots = prev })
	if err := ConfigureLocalRoots(roots); err != nil {
		t.Fatalf("ConfigureLocalRoots(%v): %v", roots, err)
	}
}

func setKubeconfigRoots(t *testing.T, roots ...string) {
	t.Helper()
	prev := permittedKubeconfigRoots
	t.Cleanup(func() { permittedKubeconfigRoots = prev })
	if err := ConfigureKubeconfigRoots(roots); err != nil {
		t.Fatalf("ConfigureKubeconfigRoots(%v): %v", roots, err)
	}
}

// mkdir creates dir (and parents) and returns it, so table setups read as data.
func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// TestLocalBasePathContainment is the boundary table for local sources: a
// base_path is usable only when it resolves inside a root the operator
// permitted. Every rejected case uses a base_path that EXISTS and is a
// directory, so a rejection can only come from the containment check.
func TestLocalBasePathContainment(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the permitted roots and the base_path to try.
		setup   func(t *testing.T) (roots []string, basePath string)
		wantErr bool
	}{
		{
			name: "base_path is exactly a permitted root",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				return []string{root}, root
			},
		},
		{
			name: "base_path is a subdirectory of a permitted root",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				return []string{root}, mkdir(t, filepath.Join(root, "envs", "prod"))
			},
		},
		{
			name: "base_path is under the second of several permitted roots",
			setup: func(t *testing.T) ([]string, string) {
				first, second := t.TempDir(), t.TempDir()
				return []string{first, second}, mkdir(t, filepath.Join(second, "team"))
			},
		},
		{
			name: "permitted root reached through a symlink",
			setup: func(t *testing.T) ([]string, string) {
				real := t.TempDir()
				link := filepath.Join(t.TempDir(), "states")
				if err := os.Symlink(real, link); err != nil {
					t.Skipf("symlinks unsupported: %v", err)
				}
				// Configured as the link, used as the link: containment must hold
				// through the link on both sides.
				return []string{link}, mkdir(t, filepath.Join(link, "envs"))
			},
		},
		{
			name: "base_path outside every permitted root",
			setup: func(t *testing.T) ([]string, string) {
				return []string{t.TempDir()}, t.TempDir()
			},
			wantErr: true,
		},
		{
			name: "sibling whose name merely extends a permitted root",
			setup: func(t *testing.T) ([]string, string) {
				parent := t.TempDir()
				root := mkdir(t, filepath.Join(parent, "states"))
				return []string{root}, mkdir(t, filepath.Join(parent, "states-evil"))
			},
			wantErr: true,
		},
		{
			name: "symlink inside a permitted root pointing outside it",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				outside := t.TempDir()
				link := filepath.Join(root, "escape")
				if err := os.Symlink(outside, link); err != nil {
					t.Skipf("symlinks unsupported: %v", err)
				}
				return []string{root}, link
			},
			wantErr: true,
		},
		{
			name: "no roots configured (fail closed)",
			setup: func(t *testing.T) ([]string, string) {
				return nil, t.TempDir()
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roots, basePath := tc.setup(t)
			setLocalRoots(t, roots...)

			_, err := New("local", map[string]any{"base_path": basePath}, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("base_path outside the permitted roots must be rejected")
				}
				// Guard against passing for the wrong reason (e.g. a missing
				// directory rather than the boundary).
				if !strings.Contains(err.Error(), "permitted") {
					t.Fatalf("expected a containment error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("base_path inside a permitted root must be accepted, got %v", err)
			}
		})
	}
}

// TestLocalContainmentKeepsKeyConfinement is the regression guard for the
// pre-existing behaviour: with the base_path itself permitted, keys are still
// confined to it — traversal segments and symlinks that leave the base are
// rejected on read, write and delete.
func TestLocalContainmentKeepsKeyConfinement(t *testing.T) {
	base := t.TempDir()
	setLocalRoots(t, base)

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	c, err := New("local", map[string]any{"base_path": base}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := c.Read(ctx, "../../etc/passwd"); err == nil {
		t.Error("traversal read must stay rejected")
	}
	if err := c.Write(ctx, "../escape.tfstate", []byte("{}")); err == nil {
		t.Error("traversal write must stay rejected")
	}
	if err := c.Delete(ctx, "../escape.tfstate"); err == nil {
		t.Error("traversal delete must stay rejected")
	}
	if _, err := c.Read(ctx, "escape/secret.tfstate"); err == nil {
		t.Error("symlink escape must stay rejected")
	}
}

// TestKubeconfigPathContainment is the same boundary for the kubernetes
// connector's kubeconfig, which is carried in the source config exactly like
// base_path. Every rejected case names a file that exists.
func TestKubeconfigPathContainment(t *testing.T) {
	writeFile := func(t *testing.T, path string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"clusters":[]}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}

	cases := []struct {
		name    string
		setup   func(t *testing.T) (roots []string, path string)
		wantErr bool
	}{
		{
			name: "kubeconfig inside a permitted root",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				return []string{root}, writeFile(t, filepath.Join(root, "config"))
			},
		},
		{
			name: "permitted root names the file itself",
			setup: func(t *testing.T) ([]string, string) {
				path := writeFile(t, filepath.Join(t.TempDir(), "config"))
				return []string{path}, path
			},
		},
		{
			name: "kubeconfig outside every permitted root",
			setup: func(t *testing.T) ([]string, string) {
				return []string{t.TempDir()}, writeFile(t, filepath.Join(t.TempDir(), "config"))
			},
			wantErr: true,
		},
		{
			name: "sibling directory whose name merely extends a permitted root",
			setup: func(t *testing.T) ([]string, string) {
				parent := t.TempDir()
				root := mkdir(t, filepath.Join(parent, "kube"))
				evil := mkdir(t, filepath.Join(parent, "kube-evil"))
				return []string{root}, writeFile(t, filepath.Join(evil, "config"))
			},
			wantErr: true,
		},
		{
			name: "symlink inside a permitted root pointing outside it",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				target := writeFile(t, filepath.Join(t.TempDir(), "config"))
				link := filepath.Join(root, "config")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlinks unsupported: %v", err)
				}
				return []string{root}, link
			},
			wantErr: true,
		},
		{
			name: "no roots configured (fail closed)",
			setup: func(t *testing.T) ([]string, string) {
				return nil, writeFile(t, filepath.Join(t.TempDir(), "config"))
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roots, path := tc.setup(t)
			setKubeconfigRoots(t, roots...)

			got, err := findKubeconfigPath(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("kubeconfig outside the permitted roots must be rejected (got %q)", got)
				}
				if !strings.Contains(err.Error(), "permitted") {
					t.Fatalf("expected a containment error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("kubeconfig inside a permitted root must be accepted, got %v", err)
			}
		})
	}
}

// TestConfigureRootsRejectsRelativePaths keeps the boundary independent of the
// process working directory: a relative root is a configuration error at boot,
// not a path silently resolved against wherever the server was launched.
func TestConfigureRootsRejectsRelativePaths(t *testing.T) {
	prevLocal, prevKube := permittedLocalRoots, permittedKubeconfigRoots
	t.Cleanup(func() { permittedLocalRoots, permittedKubeconfigRoots = prevLocal, prevKube })

	if err := ConfigureLocalRoots([]string{"states"}); err == nil {
		t.Error("relative local root must be rejected")
	}
	if err := ConfigureKubeconfigRoots([]string{"../kube"}); err == nil {
		t.Error("relative kubeconfig root must be rejected")
	}
	// Blank entries (a trailing comma in the environment variable) are ignored,
	// not treated as a root that would match everything.
	if err := ConfigureLocalRoots([]string{"/data/states", "  ", ""}); err != nil {
		t.Fatalf("ConfigureLocalRoots: %v", err)
	}
	if len(permittedLocalRoots) != 1 || permittedLocalRoots[0] != "/data/states" {
		t.Fatalf("unexpected normalized roots: %v", permittedLocalRoots)
	}
}
