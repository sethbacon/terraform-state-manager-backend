package ci

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE BACKSTOP MUST HAVE A PACKAGE FLOOR, NOT JUST A NON-EMPTY ONE.
//
// scripts/assert_integration_tests_ran.sh grades the Postgres job's transcript
// per package, against markers DERIVED from each package's integration-tagged
// files. Deriving is what makes it honest — a transcribed inventory goes stale
// silently — but the derivation reads the build tag, and so
//
//	sed -i '1s|integration|integration_disabled|' internal/approles/integration_test.go
//
// took a nineteen-test suite out of the expected set, out of the transcript, and
// out of the check, all at once. Nothing was missing, so nothing failed. That is
// #521 one level up: the guard's own enumeration is a thing that can be switched
// off, and an enumeration that found nothing looks exactly like an enumeration
// that found nothing wrong.
//
// These tests drive the REAL script, in temporary trees, and assert that each
// way of removing a suite is now loud.
//
// They run the script rather than reimplementing its rules, because a second
// copy of a check drifts from the original, which is the defect this whole area
// is about.

// backstopScript finds the script by walking up from the test's working
// directory, for the same reason repoRoot does: the module root is backend/, and
// hard-coding a relative depth breaks the moment this package moves.
func backstopScript(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		candidate := filepath.Join(dir, "scripts", "assert_integration_tests_ran.sh")
		if _, serr := os.Stat(candidate); serr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find scripts/assert_integration_tests_ran.sh above the working directory")
	return ""
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(backstopScript(t)))
}

// runBackstop invokes the script with cwd set to dir, exactly as ci.yml does
// with `working-directory: backend`. The source root is passed as `./internal`
// so the paths the script derives are module-relative and comparable with the
// manifest, again exactly as in CI.
func runBackstop(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{backstopScript(t)}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running the backstop: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// tree writes a synthetic source root: one integration-tagged suite per named
// package, plus the untagged DSN-gated marker the script also insists on. It
// returns the transcript that a fully successful run would have produced.
func tree(t *testing.T, dir string, pkgs ...string) string {
	t.Helper()
	var transcript strings.Builder
	for _, pkg := range pkgs {
		p := filepath.Join(dir, filepath.FromSlash(pkg))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		name := "TestIntegration" + strings.NewReplacer("/", "", "internal", "").Replace(pkg)
		src := "//go:build integration\n\npackage p\n\nimport \"testing\"\n\nfunc " +
			name + "(t *testing.T) {}\n"
		if err := os.WriteFile(filepath.Join(p, "integration_test.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		transcript.WriteString("=== RUN   " + name + "\n--- PASS: " + name + " (0.01s)\n")
	}

	// The untagged, DSN-gated marker: not tagged, and not named as an
	// integration file, so neither derivation may claim it.
	db := filepath.Join(dir, "internal", "db")
	if err := os.MkdirAll(db, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const marker = "TestGetMigrationVersion_ReturnsItsConnectionToThePool"
	src := "package db\n\nimport \"testing\"\n\nfunc " + marker + "(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(db, "migration_conn_leak_test.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	transcript.WriteString("--- PASS: " + marker + " (0.02s)\nPASS\n")
	return transcript.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTheBackstopFailsWhenAPackageStopsContributing(t *testing.T) {
	pkgs := []string{"internal/approles", "internal/bootstrap", "internal/tenancy"}

	cases := []struct {
		name string
		// mutate is applied after the clean tree is written. It returns the
		// transcript to grade, standing in for what CI would really have
		// captured once the suite stopped running.
		mutate   func(t *testing.T, dir, clean string) string
		wantExit int
		wantText string
	}{
		{
			name:     "the untouched tree passes",
			mutate:   func(t *testing.T, dir, clean string) string { return clean },
			wantExit: 0,
			wantText: "matching",
		},
		{
			// THE REPRODUCED AXIS. The tag is edited, the suite stops running,
			// and the transcript no longer mentions it.
			name: "a suite loses its build tag",
			mutate: func(t *testing.T, dir, clean string) string {
				path := filepath.Join(dir, "internal", "approles", "integration_test.go")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				writeFile(t, path, strings.Replace(string(body),
					"//go:build integration\n", "//go:build integration_disabled\n", 1))
				return dropPackage(clean, "approles")
			},
			wantExit: 1,
			wantText: "DISAGREE",
		},
		{
			// The same evasion by deletion: the file is gone from BOTH
			// derivations, and only the committed manifest still remembers it.
			name: "a tagged file is deleted outright",
			mutate: func(t *testing.T, dir, clean string) string {
				if err := os.RemoveAll(filepath.Join(dir, "internal", "approles")); err != nil {
					t.Fatalf("remove: %v", err)
				}
				return dropPackage(clean, "approles")
			},
			wantExit: 1,
			wantText: "EXPECTED but no longer supplying",
		},
		{
			// And by renaming everything, so that no trace of the word
			// "integration" is left to key on.
			name: "a suite is renamed out of both derivations",
			mutate: func(t *testing.T, dir, clean string) string {
				dir2 := filepath.Join(dir, "internal", "approles")
				if err := os.RemoveAll(dir2); err != nil {
					t.Fatalf("remove: %v", err)
				}
				writeFile(t, filepath.Join(dir2, "roles_test.go"),
					"package p\n\nimport \"testing\"\n\nfunc TestRoles(t *testing.T) {}\n")
				return dropPackage(clean, "approles")
			},
			wantExit: 1,
			wantText: "EXPECTED but no longer supplying",
		},
		{
			// The floor is BIDIRECTIONAL: a suite the tree supplies but the
			// manifest does not list is held to nothing, so it fails too.
			name: "a new suite is not added to the manifest",
			mutate: func(t *testing.T, dir, clean string) string {
				writeFile(t, filepath.Join(dir, "internal", "newthing", "integration_test.go"),
					"//go:build integration\n\npackage p\n\nimport \"testing\"\n\nfunc TestIntegrationNew(t *testing.T) {}\n")
				return clean + "--- PASS: TestIntegrationNew (0.01s)\n"
			},
			wantExit: 1,
			wantText: "NOT listed in the manifest",
		},
		{
			// Deleting the floor is not a way to satisfy it.
			name: "the manifest itself is emptied",
			mutate: func(t *testing.T, dir, clean string) string {
				writeFile(t, filepath.Join(dir, "manifest.txt"), "# nothing here\n")
				return clean
			},
			wantExit: 1,
			wantText: "lists no packages",
		},
		{
			// The behaviour the script already had must survive: a package
			// that is present and expected but never reported a PASS.
			name: "a listed suite builds but reports no PASS",
			mutate: func(t *testing.T, dir, clean string) string {
				return dropPackage(clean, "approles")
			},
			wantExit: 1,
			wantText: "did not build or did not run",
		},
		{
			// And the original empty-universe floor.
			name: "every suite loses its tag at once",
			mutate: func(t *testing.T, dir, clean string) string {
				if err := os.RemoveAll(filepath.Join(dir, "internal")); err != nil {
					t.Fatalf("remove: %v", err)
				}
				writeFile(t, filepath.Join(dir, "internal", "keep", "keep_test.go"), "package p\n")
				return clean
			},
			wantExit: 1,
			wantText: "found NO integration-tagged test files",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			clean := tree(t, dir, pkgs...)
			writeFile(t, filepath.Join(dir, "manifest.txt"), strings.Join(pkgs, "\n")+"\n")

			transcript := c.mutate(t, dir, clean)
			logPath := filepath.Join(dir, "pg-tests.log")
			writeFile(t, logPath, transcript)

			out, code := runBackstop(t, dir, logPath, "./internal", filepath.Join(dir, "manifest.txt"))
			if code != c.wantExit {
				t.Fatalf("exit=%d, want %d — the backstop did not notice.\n%s", code, c.wantExit, out)
			}
			if !strings.Contains(out, c.wantText) {
				t.Fatalf("output does not mention %q:\n%s", c.wantText, out)
			}
		})
	}
}

// dropPackage removes a package's PASS lines from a transcript, standing in for
// the suite no longer running at all.
func dropPackage(transcript, pkg string) string {
	var kept []string
	for _, line := range strings.Split(transcript, "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(pkg)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The manifest is only a floor if it describes THIS repository. Checked here, in
// the ordinary unit-test job, so that drift fails on every pull request rather
// than only in the Postgres job — and checked by running the script's own
// --check-manifest mode, so there is one implementation of the rule, not two.
func TestTheIntegrationPackageManifestMatchesThisTree(t *testing.T) {
	out, code := runBackstop(t, moduleRoot(t), "--check-manifest", "./internal")
	if code != 0 {
		t.Fatalf("the committed integration package manifest no longer matches the tree "+
			"(exit %d):\n%s", code, out)
	}
	t.Log(strings.TrimSpace(out))
}
