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

	// The synthetic root is a REAL module. The backstop asks the compiler which
	// files each build actually contains, and outside a module that question
	// has no answer — `go list -m` reports nothing and the script fails closed,
	// as it should. Writing a go.mod is not scaffolding placed around the
	// check; it is what makes the check reachable in these trees at all.
	writeFile(t, filepath.Join(dir, "go.mod"), "module synthetic\n\n"+goDirective(t)+"\n")

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

	// A hermetic, empty allowlist. Passing it explicitly keeps these trees
	// independent of the repository's real integration_ignored_files.txt: an
	// entry added there for the real tree must not leak into synthetic ones.
	writeFile(t, filepath.Join(dir, "allowlist.txt"), "# no exceptions\n")
	return transcript.String()
}

// goDirective returns the `go` line of the module under test, so the synthetic
// modules below are accepted by exactly the toolchain that builds this
// repository. A hard-coded version would go stale at the next bump, and omitting
// the directive would leave the module at go1.16 semantics.
func goDirective(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "go ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatal("the module under test has no `go` directive")
	return ""
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

			out, code := runBackstop(t, dir, logPath, "./internal",
				filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
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

// A FILE CAN LEAVE THE BUILD WITHOUT LEAVING THE SOURCE.
//
// Both derivations above read the source and match TEXT against the build
// constraint, so the constraint never has to lose the word `integration` in
// order to stop selecting the file. It only has to GAIN a second term that
// nothing sets:
//
//	//go:build integration    ->    //go:build integration && postgres
//
// Derivation A still matches `integration` as a whole word. Derivation B still
// reads the unchanged filename. The two agree file-for-file, so FLOOR TWO is
// satisfied. The package keeps its other tagged files, so it still reports a
// top-level PASS and the grading loop is satisfied. And integration_packages.txt
// is PACKAGE-granular while the loss is FILE-granular, so FLOOR THREE cannot see
// it at any level of manifest care. The guard exited 0 with
// internal/tenancy/isolation_integration_test.go — the cross-tenant leak proof
// for #393 — silently out of the build.
//
// So every tree here gives internal/tenancy TWO tagged files and mutates only
// one of them, and grades a transcript in which the surviving sibling still
// passes. A check that merely re-derives the package set cannot pass these by
// accident; only asking the compiler, per file, can.
func TestTheBackstopSeesOneFileLeaveTheIntegrationBuild(t *testing.T) {
	const isolation = "isolation_integration_test.go"

	// reconstrain rewrites the first line of the second tenancy file, leaving
	// every other byte — and every other file — exactly as it was.
	reconstrain := func(constraint string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			path := filepath.Join(dir, "internal", "tenancy", isolation)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			lines := strings.Split(string(body), "\n")
			lines[0] = constraint
			writeFile(t, path, strings.Join(lines, "\n"))

			// PROVE THE MUTATION APPLIED, AND THAT IT DID WHAT IT CLAIMS. A
			// rewrite that silently missed, or a spelling the toolchain happens
			// to accept anyway, would leave the assertion below testing nothing.
			if got := firstLine(t, path); got != constraint {
				t.Fatalf("the constraint rewrite did not apply: first line is %q, want %q", got, constraint)
			}
			if compiledUnderIntegrationTag(t, dir, "./internal/tenancy", isolation) {
				t.Fatalf("%s is STILL in the -tags integration build under %q — "+
					"this case is not exercising the defect", isolation, constraint)
			}
		}
	}

	cases := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantExit int
		wantText string
	}{
		{
			// The floor must not be satisfied by failing on everything.
			name:     "the two-file tree passes untouched",
			mutate:   func(t *testing.T, dir string) {},
			wantExit: 0,
			wantText: "matching",
		},
		{
			name:     "a second term that nothing in CI sets",
			mutate:   reconstrain("//go:build integration && postgres"),
			wantExit: 1,
			wantText: "NOT in the build under -tags integration",
		},
		{
			name:     "the older +build spelling of the same idea",
			mutate:   reconstrain("// +build integration,postgres"),
			wantExit: 1,
			wantText: "NOT in the build under -tags integration",
		},
		{
			name:     "a negated term that can never be true",
			mutate:   reconstrain("//go:build integration && !integration"),
			wantExit: 1,
			wantText: "NOT in the build under -tags integration",
		},
		{
			name:     "a term under a name nothing anywhere sets",
			mutate:   reconstrain("//go:build integration && requiresrealpostgres"),
			wantExit: 1,
			wantText: "NOT in the build under -tags integration",
		},
		{
			// FLOOR FIVE. The same trick on a file that reads as an ordinary
			// unit test: it is in neither build, so nothing it asserts ever
			// runs, and neither text derivation has any reason to look at it.
			name: "a test file gated behind a term no build passes",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "slow_test.go"),
					"//go:build slowdb\n\npackage p\n\nimport \"testing\"\n\nfunc TestSlow(t *testing.T) {}\n")
			},
			wantExit: 1,
			wantText: "in NEITHER build",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			withIsolation, withoutIsolation := twoFileTree(t, dir)

			transcript := withIsolation
			if c.wantExit != 0 {
				// What CI would really have captured: the mutated file did not
				// compile, so its test never reported — and its sibling did, so
				// the PACKAGE still reports and grading still passes.
				transcript = withoutIsolation
			}
			logPath := filepath.Join(dir, "pg-tests.log")
			writeFile(t, logPath, transcript)

			c.mutate(t, dir)

			out, code := runBackstop(t, dir, logPath, "./internal",
				filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
			if code != c.wantExit {
				t.Fatalf("exit=%d, want %d — the backstop did not notice.\n%s", code, c.wantExit, out)
			}
			if !strings.Contains(out, c.wantText) {
				t.Fatalf("output does not mention %q:\n%s", c.wantText, out)
			}
			if c.wantExit != 0 && !strings.Contains(out, "internal/tenancy/") {
				t.Fatalf("the failure does not NAME the file that left the build, "+
					"which is the whole point of a file-granular floor:\n%s", out)
			}
		})
	}
}

// twoFileTree writes the clean tree and gives internal/tenancy a SECOND tagged
// file, mirroring internal/tenancy, which has six. It returns the transcript of
// a run in which both tenancy files reported, and the transcript of one in which
// only the sibling did.
func twoFileTree(t *testing.T, dir string) (withIsolation, withoutIsolation string) {
	t.Helper()
	pkgs := []string{"internal/approles", "internal/tenancy"}
	base := tree(t, dir, pkgs...)
	writeFile(t, filepath.Join(dir, "internal", "tenancy", "isolation_integration_test.go"),
		"//go:build integration\n\npackage p\n\nimport \"testing\"\n\nfunc TestIntegrationIsolation(t *testing.T) {}\n")
	writeFile(t, filepath.Join(dir, "manifest.txt"), strings.Join(pkgs, "\n")+"\n")
	return base + "--- PASS: TestIntegrationIsolation (0.01s)\n", base
}

func firstLine(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.SplitN(string(body), "\n", 2)[0]
}

// compiledUnderIntegrationTag asks the toolchain the same question the backstop
// asks, independently of the backstop, so a case cannot claim to have removed a
// file from the build without that having actually happened.
func compiledUnderIntegrationTag(t *testing.T, dir, pkg, file string) bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "integration",
		"-f", "{{range .TestGoFiles}}{{.}}\n{{end}}", pkg)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in the synthetic tree: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == file {
			return true
		}
	}
	return false
}

// THE TOOLCHAIN FLOOR MUST NOT PASS ON AN EMPTY UNIVERSE.
//
// `go list ./internal/...` matching nothing is a WARNING on stderr and an exit
// status of zero, so a root the toolchain cannot see produces empty file sets
// and no error at all. That is the shape of every bug in this file: nothing
// found reads exactly like nothing wrong. Here the source root holds a properly
// tagged, properly named suite that matches the manifest — so floors one, two
// and three are all satisfied — but it sits under a directory the `...` pattern
// skips, so the compiler reports no packages whatsoever.
func TestTheToolchainFloorRefusesAnEmptyUniverse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module synthetic\n\n"+goDirective(t)+"\n")

	// `testdata` is skipped by the `...` pattern, which is precisely why grep
	// can see this file and the compiler cannot.
	writeFile(t, filepath.Join(dir, "internal", "testdata", "tenancy", "isolation_integration_test.go"),
		"//go:build integration\n\npackage p\n\nimport \"testing\"\n\nfunc TestIntegrationIsolation(t *testing.T) {}\n")
	writeFile(t, filepath.Join(dir, "manifest.txt"), "internal/testdata/tenancy\n")
	writeFile(t, filepath.Join(dir, "allowlist.txt"), "# no exceptions\n")

	out, code := runBackstop(t, dir, "--check-manifest", "./internal",
		filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
	if code == 0 {
		t.Fatalf("the backstop passed with an empty compiled set, so the toolchain floor is vacuous:\n%s", out)
	}
	if !strings.Contains(out, "reported NO test files") {
		t.Fatalf("output does not say the compiled set was empty:\n%s", out)
	}
}

// A PROOF CAN BE NEUTERED WITHOUT TOUCHING A TEST FILE AT ALL.
//
// Every floor above floor six inspects TEST files: the tag derivation, the name
// derivation, the manifest, the compiler's gated set and the neither-build
// sweep all ask about *_test.go. So the never-set second term did not have to
// live in a test file. Relocate the load-bearing implementation into a NON-test
// pair —
//
//	//go:build postgres     (the real implementation)
//	//go:build !postgres    (a no-op stub)
//
// — and the test file keeps its tag, its name, its compiled membership and its
// PASS marker, while the thing it exercises has been swapped for the stub.
// Demonstrated against this script before floor six existed: the guard exited
// 0 on exactly the trees below.
//
// The fix is again the compiler, not another pattern: `go list -tags
// integration` reports the files it EXCLUDED per package as IgnoredGoFiles,
// and a filesystem sweep catches the directory `./...` silently drops when ALL
// of its files are excluded — that variant was demonstrated against the first
// draft of the fix. Anything excluded is either named, with a reason, in the
// committed allowlist, or a failure naming the file. Platform-gated files are
// deliberately NOT auto-tolerated: `windows` with a `!windows` stub is the
// same relocation wearing a GOOS name, so a legitimate platform file is
// admitted the same way — one allowlist line a reviewer can weigh.
func TestTheBackstopSeesARelocatedImplementationLeaveTheBuild(t *testing.T) {
	const realImpl = "//go:build postgres\n\npackage p\n\n// Real is the load-bearing implementation.\nfunc Real() int { return 42 }\n"
	const stubImpl = "//go:build !postgres\n\npackage p\n\n// Real is a no-op stand-in; nothing sets `postgres`, so THIS is what CI builds.\nfunc Real() int { return 0 }\n"

	cases := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantExit int
		wantText []string
	}{
		{
			// THE REPRODUCED AXIS. Both files sit in a package that still has
			// compiling files, so the exclusion shows up as IgnoredGoFiles.
			name: "a stub pair parks the real implementation behind a term nothing sets",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "probe_postgres.go"), realImpl)
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "probe_stub.go"), stubImpl)
				// PROVE THE MUTATION DID WHAT IT CLAIMS: the compiler must be
				// building the stub and excluding the real file, or this case
				// is not exercising the defect.
				if !containsFile(goListField(t, dir, "./internal/tenancy", "GoFiles"), "probe_stub.go") {
					t.Fatal("probe_stub.go is not in the -tags integration build — the stub is not standing in")
				}
				if !containsFile(goListField(t, dir, "./internal/tenancy", "IgnoredGoFiles"), "probe_postgres.go") {
					t.Fatal("probe_postgres.go is not ignored under -tags integration — the real implementation never left the build")
				}
			},
			wantExit: 1,
			wantText: []string{"EXCLUDED from the build", "internal/tenancy/probe_postgres.go"},
		},
		{
			// THE VANISHED DIRECTORY. `go list ./...` silently skips a
			// directory whose files are ALL excluded — no package, no
			// IgnoredGoFiles, no error — so IgnoredGoFiles alone missed this.
			name: "the same relocation parked alone in a directory the compiler drops",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "pgonly", "probe.go"), realImpl)
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "probe_stub.go"), stubImpl)
				// PROVE the directory really is invisible to the compiler.
				for _, ip := range goListImportPaths(t, dir) {
					if strings.HasSuffix(ip, "internal/pgonly") {
						t.Fatalf("%s is still reported by go list — this case is not exercising the blind spot", ip)
					}
				}
			},
			wantExit: 1,
			wantText: []string{"internal/pgonly/probe.go", "NO package for"},
		},
		{
			// GOOS files land in the floor deliberately: unaccounted, they fail.
			name: "a platform-named file is not auto-tolerated",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "conn_windows.go"),
					"package p\n\nfunc winProbe() int { return 7 }\n")
				if !containsFile(goListField(t, dir, "./internal/tenancy", "IgnoredGoFiles"), "conn_windows.go") {
					t.Fatal("conn_windows.go is not ignored under the pinned linux build — premise broken")
				}
			},
			wantExit: 1,
			wantText: []string{"internal/tenancy/conn_windows.go", "GOOS/GOARCH"},
		},
		{
			// ...and admitted by ONE reviewable line with a reason, so a
			// legitimate platform file does not leave the guard crying wolf.
			name: "a justified platform file is admitted by the allowlist",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "conn_windows.go"),
					"package p\n\nfunc winProbe() int { return 7 }\n")
				writeFile(t, filepath.Join(dir, "allowlist.txt"),
					"internal/tenancy/conn_windows.go  # windows-only shim; CI's linux build never runs it\n")
			},
			wantExit: 0,
			wantText: []string{"matching"},
		},
		{
			// The reason is the reviewable half of the contract, not decoration.
			name: "an allowlist entry without a reason is refused",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "conn_windows.go"),
					"package p\n\nfunc winProbe() int { return 7 }\n")
				writeFile(t, filepath.Join(dir, "allowlist.txt"),
					"internal/tenancy/conn_windows.go\n")
			},
			wantExit: 1,
			wantText: []string{"carries no reason"},
		},
		{
			// The comparison is BOTH ways: an entry naming a file that is not
			// excluded is a door left propped open for a future hole.
			name: "a stale allowlist entry is refused",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "allowlist.txt"),
					"internal/tenancy/integration_test.go  # pre-planted excuse for a file that is in the build\n")
			},
			wantExit: 1,
			wantText: []string{"NOT excluded", "internal/tenancy/integration_test.go"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// The transcript is the FULLY GREEN one: every tagged test still
			// compiles, runs and passes — against the stub. Only floor six can
			// tell this tree from an honest one.
			withIsolation, _ := twoFileTree(t, dir)
			logPath := filepath.Join(dir, "pg-tests.log")
			writeFile(t, logPath, withIsolation)

			c.mutate(t, dir)

			out, code := runBackstop(t, dir, logPath, "./internal",
				filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
			if code != c.wantExit {
				t.Fatalf("exit=%d, want %d — the backstop did not notice.\n%s", code, c.wantExit, out)
			}
			for _, want := range c.wantText {
				if !strings.Contains(out, want) {
					t.Fatalf("output does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// ONE PASS PER FILE, NOT PER PACKAGE — AND PROVED AGAINST REAL TRANSCRIPTS.
//
// A package's tagged files share a test binary, so under the per-package marker
// a sibling file's PASS covered a file whose tests were skipped or filtered
// away: the reported marker silently swapped, and the guard exited 0 on both
// transcripts below. Now every integration-gated file must contribute a
// top-level PASS from a function it declares.
//
// These cases do not hand-write the failing transcripts: they run the REAL
// `go test -tags integration` in the synthetic module and grade its actual
// output, so the grading is proved against what the toolchain really prints —
// a hand-written transcript could drift from that format and these tests would
// never know.
func TestEachIntegrationGatedFileMustContributeItsOwnPass(t *testing.T) {
	cases := []struct {
		name string
		// prepare edits the tree and returns the `go test` arguments beyond
		// the standard ones.
		prepare      func(t *testing.T, dir string) []string
		wantExit     int
		wantText     []string
		requireInLog string
	}{
		{
			name:     "a real full run passes",
			prepare:  func(t *testing.T, dir string) []string { return nil },
			wantExit: 0,
			wantText: []string{"matching"},
		},
		{
			// FALSIFICATION (b): every test in the isolation file skips. The
			// file still compiles — floors 1-7 are all satisfied — and its
			// sibling in the SAME package still passes, which used to be the
			// whole package's marker.
			name: "a file whose tests all skip no longer hides behind its sibling",
			prepare: func(t *testing.T, dir string) []string {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "isolation_integration_test.go"),
					"//go:build integration\n\npackage p\n\nimport \"testing\"\n\n"+
						"func TestIntegrationIsolation(t *testing.T) { t.Skip(\"silenced\") }\n")
				return nil
			},
			wantExit:     1,
			wantText:     []string{"internal/tenancy/isolation_integration_test.go", "did not build or did not run"},
			requireInLog: "--- SKIP: TestIntegrationIsolation",
		},
		{
			// A -run filter that silences the whole file: nothing is skipped,
			// nothing fails, the file's tests simply never appear.
			name: "a -run filter that silences a whole file is caught",
			prepare: func(t *testing.T, dir string) []string {
				return []string{"-run", "^TestIntegrationapproles$|^TestIntegrationtenancy$|^TestGetMigrationVersion_ReturnsItsConnectionToThePool$"}
			},
			wantExit: 1,
			wantText: []string{"internal/tenancy/isolation_integration_test.go", "did not build or did not run"},
		},
		{
			// THE ROUND-2 FORGERY, run for real rather than synthesised: every
			// test in the gated file skips, and a sibling test in the same
			// package prints the file's marker to os.Stdout at column 0 as a
			// top-level PASS line. Before the contradiction check, go test
			// exited 0 and the guard graded the forged line as the file's
			// proof. The transcript necessarily carries BOTH the forged PASS
			// and the real SKIP for the same name -- go test emits exactly one
			// top-level verdict per function -- and that contradiction is what
			// the guard now refuses.
			name: "a forged PASS printed beside the real SKIP is refused",
			prepare: func(t *testing.T, dir string) []string {
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "isolation_integration_test.go"),
					"//go:build integration\n\npackage p\n\nimport \"testing\"\n\n"+
						"func TestIntegrationIsolation(t *testing.T) { t.Skip(\"silenced\") }\n")
				writeFile(t, filepath.Join(dir, "internal", "tenancy", "integration_test.go"),
					"//go:build integration\n\npackage p\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\n"+
						"func TestIntegrationtenancy(t *testing.T) {\n"+
						"\tfmt.Print(\"--- PASS: TestIntegrationIsolation (0.00s)\\n\")\n"+
						"}\n")
				return nil
			},
			wantExit:     1,
			wantText:     []string{"BOTH a PASS and a SKIP or FAIL", "TestIntegrationIsolation"},
			requireInLog: "--- SKIP: TestIntegrationIsolation",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			twoFileTree(t, dir)
			extraArgs := c.prepare(t, dir)

			transcript := goTestTranscript(t, dir, extraArgs...)
			if c.requireInLog != "" && !strings.Contains(transcript, c.requireInLog) {
				t.Fatalf("the real `go test` transcript does not contain %q — the premise of this case did not hold:\n%s",
					c.requireInLog, transcript)
			}
			logPath := filepath.Join(dir, "pg-tests.log")
			writeFile(t, logPath, transcript)

			out, code := runBackstop(t, dir, logPath, "./internal",
				filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
			if code != c.wantExit {
				t.Fatalf("exit=%d, want %d — the backstop did not notice.\n%s", code, c.wantExit, out)
			}
			for _, want := range c.wantText {
				if !strings.Contains(out, want) {
					t.Fatalf("output does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// The per-file grading keys on test-function names in a transcript that never
// says which package printed a line. A marker name declared twice anywhere in
// the same `go test` run is therefore ambiguous evidence — one file's PASS
// could stand in for the other's silence — so it is refused outright.
func TestAMarkerNameDeclaredTwiceInTheTaggedBuildIsRefused(t *testing.T) {
	dir := t.TempDir()
	twoFileTree(t, dir)
	// The gated file's marker keeps the integration NAME convention via its
	// filename; the duplicate lives in a plain unit-test file of another
	// package, which no earlier floor has any reason to look at.
	writeFile(t, filepath.Join(dir, "internal", "tenancy", "isolation_integration_test.go"),
		"//go:build integration\n\npackage p\n\nimport \"testing\"\n\nfunc TestCrossTenantIsolation(t *testing.T) {}\n")
	writeFile(t, filepath.Join(dir, "internal", "approles", "extra_test.go"),
		"package p\n\nimport \"testing\"\n\nfunc TestCrossTenantIsolation(t *testing.T) {}\n")

	out, code := runBackstop(t, dir, "--check-manifest", "./internal",
		filepath.Join(dir, "manifest.txt"), filepath.Join(dir, "allowlist.txt"))
	if code == 0 {
		t.Fatalf("the backstop accepted a duplicated marker name, so a PASS line cannot be attributed:\n%s", out)
	}
	for _, want := range []string{
		"declared more than once",
		"internal/tenancy/isolation_integration_test.go",
		"internal/approles/extra_test.go",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not mention %q:\n%s", want, out)
		}
	}
}

// goListField asks the toolchain for one file-list field of one package in the
// synthetic tree, pinned to the platform the backstop pins (linux/amd64), so a
// case can prove its mutation did what it claims independently of the script
// under test.
func goListField(t *testing.T, dir, pkg, field string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "integration",
		"-f", "{{range ."+field+"}}{{.}}\n{{end}}", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s in the synthetic tree: %v", pkg, err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.TrimSpace(line); f != "" {
			files = append(files, f)
		}
	}
	return files
}

func goListImportPaths(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "integration", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./... in the synthetic tree: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func containsFile(files []string, want string) bool {
	for _, f := range files {
		if f == want {
			return true
		}
	}
	return false
}

// goTestTranscript runs the REAL `go test -tags integration -v` over the
// synthetic module and returns its combined output — the same thing ci.yml
// tees into pg-tests.log. A non-zero exit is not fatal by itself: a skipped
// suite exits 0 and a failing one is a legitimate transcript to grade; only a
// run that produced no `--- ` lines at all is treated as broken scaffolding.
func goTestTranscript(t *testing.T, dir string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{"test", "-tags", "integration", "-v", "-count=1"}, extraArgs...)
	args = append(args, "./internal/...")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "--- ") {
		t.Fatalf("go test in the synthetic tree produced no gradable output: %v\n%s", err, out)
	}
	return string(out)
}
