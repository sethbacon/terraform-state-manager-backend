package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A `run:` STEP MUST GRADE ITS WHOLE PIPELINE.
//
// GitHub's default shell, used whenever neither the step, the job, nor the
// workflow says otherwise, is `bash -e {0}` — which has NO pipefail. A step
// spelled `some-check | tee log` therefore exits with tee's status, tee always
// succeeds, and the check's failure is discarded. The step is green, the job is
// green, and the required check reports success.
//
// That is #521, and it was not theoretical: `go test ./internal/... -tags
// integration -v 2>&1 | tee /tmp/pg-tests.log` reported success on every run for
// two days while internal/approles did not compile under the tag at all.
//
// # Why this is a guard and not a fixed workflow
//
// Three of the file's piped steps already spelled `set -euo pipefail` in their
// own bodies. The idiom was established; one step simply did not use it. A guard
// that only fixed that step would leave the next one to remember — and the whole
// lesson of #521 is that a protection which can be omitted eventually is.
//
// So the property asserted is per-STEP and universal: every `run:` step reaches
// the shell under pipefail, whether it contains a pipe today or not. A step with
// no pipe costs nothing to comply, and complying is what stops a pipe added
// later from silently losing its grading.
//
// # Why it parses YAML rather than grepping
//
// A regex over the text can only see the spellings it was written for. `run:`
// can be written as `"run":`, a step can be a flow mapping on one line, and a
// shell can be quoted — all of which a text scan misses ENTIRELY rather than
// reporting, which is the same blind-versus-clean failure one level up.
func TestEveryWorkflowRunStepGetsPipefail(t *testing.T) {
	root := repoRoot(t)

	audited, findings, err := auditRunStepShells(root)
	if err != nil {
		t.Fatalf("auditing %s: %v", root, err)
	}

	// FLOOR. An enumeration that found nothing looks exactly like an
	// enumeration that found nothing wrong — the bug class this file exists to
	// close. TestTheShellAuditRefusesAnEmptyUniverse falsifies it.
	if audited == 0 {
		t.Fatalf("audited NO run steps under %s/.github — either the layout moved or this "+
			"enumeration broke, and in both cases a green result here means nothing", root)
	}
	t.Logf("audited %d run step(s)", audited)

	if len(findings) > 0 {
		t.Fatalf("these run steps reach the shell WITHOUT pipefail, so a failing command in a "+
			"pipeline would be discarded and the step would report success (#521):\n  %s\n\n"+
			"Fix at the workflow level:\n\ndefaults:\n  run:\n    shell: bash -euo pipefail {0}\n\n"+
			"The bare `bash` keyword also works — GitHub documents it as "+
			"`bash --noprofile --norc -eo pipefail {0}`. A composite action takes no workflow "+
			"defaults, so its steps must say it themselves.",
			strings.Join(findings, "\n  "))
	}
}

// THE FLOOR IS FALSIFIED, not merely asserted. Handed a tree with no workflows
// in it, the audit must ERROR rather than return a clean, empty result — because
// "nothing to check" and "everything checks out" are the two states this whole
// file exists to keep apart.
func TestTheShellAuditRefusesAnEmptyUniverse(t *testing.T) {
	empty := t.TempDir()

	if _, _, err := auditRunStepShells(empty); err == nil {
		t.Fatal("the audit accepted a tree with no .github directory and reported no error; " +
			"an empty universe must fail, not pass vacuously")
	}

	// A .github/workflows that exists but holds nothing must fail the same way:
	// deleting the workflows is not a way to satisfy the guard.
	if err := os.MkdirAll(filepath.Join(empty, ".github", "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := auditRunStepShells(empty); err == nil {
		t.Fatal("the audit accepted an EMPTY .github/workflows and reported no error")
	}
}

// The audit must report a non-compliant step rather than fail to see it. Each
// case is a spelling that a text scan would plausibly miss: quoted keys, a flow
// mapping, a step-level shell that OVERRIDES a good workflow default, and a job
// default that does the same.
func TestTheShellAuditSeesEverySpellingOfANonCompliantStep(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantHit bool
	}{
		{
			name:    "no defaults at all",
			yaml:    "jobs:\n  a:\n    steps:\n      - run: echo hi | tee log\n",
			wantHit: true,
		},
		{
			name:    "workflow default supplies pipefail",
			yaml:    "defaults:\n  run:\n    shell: bash -euo pipefail {0}\njobs:\n  a:\n    steps:\n      - run: echo hi | tee log\n",
			wantHit: false,
		},
		{
			name:    "the bare bash keyword supplies pipefail",
			yaml:    "defaults:\n  run:\n    shell: bash\njobs:\n  a:\n    steps:\n      - run: echo hi\n",
			wantHit: false,
		},
		{
			// The spelling that started this: it names bash, and it is exactly
			// GitHub's unguarded default.
			name:    "a shell that names bash but not pipefail",
			yaml:    "defaults:\n  run:\n    shell: bash -e {0}\njobs:\n  a:\n    steps:\n      - run: echo hi\n",
			wantHit: true,
		},
		{
			name:    "a step-level shell overriding a good workflow default",
			yaml:    "defaults:\n  run:\n    shell: bash -euo pipefail {0}\njobs:\n  a:\n    steps:\n      - run: echo hi\n        shell: sh\n",
			wantHit: true,
		},
		{
			name:    "a job-level default overriding a good workflow default",
			yaml:    "defaults:\n  run:\n    shell: bash -euo pipefail {0}\njobs:\n  a:\n    defaults:\n      run:\n        shell: sh\n    steps:\n      - run: echo hi\n",
			wantHit: true,
		},
		{
			// QUOTED IDENTIFIERS. The blind axis a regex-based guard has: the
			// same statement, spelled so the pattern cannot match it at all.
			name:    "quoted keys",
			yaml:    "\"jobs\":\n  \"a\":\n    \"steps\":\n      - \"run\": \"echo hi | tee log\"\n",
			wantHit: true,
		},
		{
			name:    "a step written as a flow mapping on one line",
			yaml:    "jobs:\n  a:\n    steps: [{name: x, run: \"echo hi | tee log\"}]\n",
			wantHit: true,
		},
		{
			name:    "the body sets pipefail itself",
			yaml:    "jobs:\n  a:\n    steps:\n      - run: |\n          set -euo pipefail\n          echo hi | tee log\n",
			wantHit: false,
		},
		{
			// A shell COMMENT that merely mentions the idiom is not the idiom.
			name:    "the body only talks about pipefail in a comment",
			yaml:    "jobs:\n  a:\n    steps:\n      - run: |\n          # we should set -euo pipefail here one day\n          echo hi | tee log\n",
			wantHit: true,
		},
		{
			// A composite action takes NO workflow defaults, so a `defaults`
			// block in it protects nothing.
			name:    "a composite action step with no shell",
			yaml:    "defaults:\n  run:\n    shell: bash\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n",
			wantHit: true,
		},
		{
			name:    "a composite action step that names its own shell",
			yaml:    "runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n",
			wantHit: false,
		},
		{
			name:    "a job that calls a reusable workflow has no steps to audit",
			yaml:    "jobs:\n  a:\n    uses: ./.github/workflows/ci.yml\n",
			wantHit: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			wf := filepath.Join(dir, ".github", "workflows")
			if err := os.MkdirAll(wf, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(wf, "probe.yml"), []byte(c.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			audited, findings, err := auditRunStepShells(dir)
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			// The reusable-workflow case legitimately audits nothing; every
			// other case must have found a step to judge, or the audit is
			// blind rather than satisfied.
			if c.name != "a job that calls a reusable workflow has no steps to audit" && audited == 0 {
				t.Fatalf("the audit saw NO run step in this spelling — it is blind to it, not clean on it:\n%s", c.yaml)
			}
			if got := len(findings) > 0; got != c.wantHit {
				t.Fatalf("findings=%v (%v), want a finding=%v for:\n%s", got, findings, c.wantHit, c.yaml)
			}
		})
	}
}

// ── the audit ────────────────────────────────────────────────────────────────

type ymlDefaults struct {
	Run struct {
		Shell string `yaml:"shell"`
	} `yaml:"run"`
}

type ymlStep struct {
	Name  string `yaml:"name"`
	Run   string `yaml:"run"`
	Shell string `yaml:"shell"`
}

type ymlJob struct {
	Name     string      `yaml:"name"`
	Defaults ymlDefaults `yaml:"defaults"`
	Steps    []ymlStep   `yaml:"steps"`
}

type ymlFile struct {
	Defaults ymlDefaults       `yaml:"defaults"`
	Jobs     map[string]ymlJob `yaml:"jobs"`
	Runs     *struct {
		Using string    `yaml:"using"`
		Steps []ymlStep `yaml:"steps"`
	} `yaml:"runs"`
}

// auditRunStepShells returns how many `run:` steps it judged and a finding for
// each one that reaches the shell without pipefail.
//
// It errors — rather than returning (0, nil, nil) — when there is nothing to
// audit, because a caller cannot tell a clean tree from an unreadable one by the
// finding count alone.
func auditRunStepShells(root string) (audited int, findings []string, err error) {
	dir := filepath.Join(root, ".github")
	files, err := workflowAndActionFiles(dir)
	if err != nil {
		return 0, nil, err
	}
	if len(files) == 0 {
		return 0, nil, &auditError{"no workflow or composite-action files found under " + dir}
	}

	for _, path := range files {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return 0, nil, rerr
		}
		var f ymlFile
		if uerr := yaml.Unmarshal(raw, &f); uerr != nil {
			return 0, nil, &auditError{path + ": " + uerr.Error()}
		}
		rel, _ := filepath.Rel(root, path)

		// A composite action does NOT inherit a caller's `defaults`, so its
		// steps are judged on their own shell alone.
		if f.Runs != nil {
			for i, s := range f.Runs.Steps {
				if strings.TrimSpace(s.Run) == "" {
					continue
				}
				audited++
				if !stepIsGraded(s.Shell, s.Run) {
					findings = append(findings, describe(rel, "runs", i, s))
				}
			}
		}

		jobIDs := make([]string, 0, len(f.Jobs))
		for id := range f.Jobs {
			jobIDs = append(jobIDs, id)
		}
		sort.Strings(jobIDs)

		for _, id := range jobIDs {
			job := f.Jobs[id]
			for i, s := range job.Steps {
				if strings.TrimSpace(s.Run) == "" {
					continue
				}
				audited++
				// Most specific wins, exactly as GitHub resolves it.
				shell := firstNonEmpty(s.Shell, job.Defaults.Run.Shell, f.Defaults.Run.Shell)
				if !stepIsGraded(shell, s.Run) {
					findings = append(findings, describe(rel, id, i, s))
				}
			}
		}
	}
	return audited, findings, nil
}

type auditError struct{ msg string }

func (e *auditError) Error() string { return e.msg }

func workflowAndActionFiles(githubDir string) ([]string, error) {
	info, err := os.Stat(githubDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &auditError{githubDir + " is not a directory"}
	}
	var out []string
	walkErr := filepath.WalkDir(githubDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}
		rel, rerr := filepath.Rel(githubDir, path)
		if rerr != nil {
			return rerr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case len(parts) == 2 && parts[0] == "workflows":
			out = append(out, path)
		case filepath.Base(path) == "action.yml" || filepath.Base(path) == "action.yaml":
			out = append(out, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(out)
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// bodyPipefail matches an actual `set` statement enabling pipefail, in either
// the `-euo pipefail` or the `-o pipefail` spelling.
var bodyPipefail = regexp.MustCompile(`(^|;|&&|\|\|)\s*set\s+-[A-Za-z]*o?[A-Za-z]*\s+pipefail|(^|;|&&|\|\|)\s*set\s+-o\s+pipefail`)

// stepIsGraded reports whether this step's pipelines are graded: either the
// shell it runs under enables pipefail, or its own first statements do.
func stepIsGraded(shell, body string) bool {
	if shellEnablesPipefail(shell) {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if bodyPipefail.MatchString(l) {
			return true
		}
	}
	return false
}

// shellEnablesPipefail decides from the `shell:` value alone.
//
// An empty value is GitHub's unspecified default, `bash -e {0}` — the one
// without pipefail, and the reason this guard exists. The bare keyword `bash` is
// documented as `bash --noprofile --norc -eo pipefail {0}`, so it counts; a
// custom command line counts only if it actually says pipefail, which is why
// `bash -e {0}` does not pass merely for containing the word bash.
func shellEnablesPipefail(shell string) bool {
	s := strings.TrimSpace(shell)
	switch {
	case s == "":
		return false
	case s == "bash":
		return true
	default:
		return strings.Contains(s, "pipefail")
	}
}

func describe(file, job string, index int, s ymlStep) string {
	name := s.Name
	if name == "" {
		name = strings.SplitN(strings.TrimSpace(s.Run), "\n", 2)[0]
		if len(name) > 60 {
			name = name[:60] + "…"
		}
	}
	return file + " job " + job + " step " + strconv.Itoa(index) + ": " + name
}

// repoRoot walks up from the test's working directory to the tree that holds
// .github. The Go module root is backend/, so the workflows are one level above
// it, and hard-coding "../.." would break the moment this package moves.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if info, serr := os.Stat(filepath.Join(dir, ".github", "workflows")); serr == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repository root (no .github/workflows above the working directory)")
	return ""
}
