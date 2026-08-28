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

// THE FLOORS OF THE SCAN ITSELF.
//
// An enumeration that collapses reports the same thing as an enumeration that
// found nothing wrong, so the counts are ASSERTED, not printed. Both numbers are
// the repository's actual content at the time they were written: raise them when
// files are added, and lower one only as a deliberate, reviewable edit that says
// in the diff which CI file stopped existing.
//
// minActionFiles is kept separate from minWorkflowFiles on purpose. The
// composite-action half of the walk and the workflow half fail independently --
// that is exactly what happened before this guard was widened, when the walk
// looked only under .github and never saw an action.yml anywhere else -- and one
// combined total would let a healthy workflow count hide a dead action scan.
const (
	minWorkflowFiles = 7
	minActionFiles   = 1
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
// # WHAT THIS GUARD PROMISES, EXACTLY
//
// PIPEFAIL, AND ONLY PIPEFAIL. It does not check `-e`, and it deliberately does
// NOT check `-u`. A step may be spelled `bash -o pipefail {0}`, with no nounset
// anywhere, and pass here — that is intended, not an oversight. #521 was a lost
// pipeline exit status; unset-variable behaviour is a different property with a
// different failure mode, and folding it in would mean this guard's name no
// longer described what a green result proved. Anyone reading a pass here should
// read it as "every run step grades its pipelines", and nothing more. If nounset
// is wanted repo-wide, it wants its own guard.
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
//
// # Why it walks the whole repository
//
// A composite action is not a .github thing. `uses: ./tools/myaction` resolves
// an action.yml at any path, and a repository that publishes an action keeps one
// at its ROOT. Both spellings ran ungraded steps past the first version of this
// guard, which enumerated under .github only. The walk now covers the whole
// tree, and minActionFiles above keeps that half of it from quietly dying.
func TestEveryWorkflowRunStepGetsPipefail(t *testing.T) {
	root := repoRoot(t)

	res, err := auditRunStepShells(root)
	if err != nil {
		t.Fatalf("auditing %s: %v", root, err)
	}

	// FLOOR. An enumeration that found nothing looks exactly like an
	// enumeration that found nothing wrong — the bug class this file exists to
	// close. TestTheShellAuditRefusesAnEmptyUniverse falsifies it.
	if res.Steps == 0 {
		t.Fatalf("audited NO run steps under %s — either the layout moved or this "+
			"enumeration broke, and in both cases a green result here means nothing", root)
	}
	if res.Workflows < minWorkflowFiles {
		t.Fatalf("scanned only %d workflow file(s) under %s/.github/workflows, want at least %d: "+
			"the walk has collapsed, and an audit that reads nothing reports the same clean "+
			"result as an audit that found nothing wrong", res.Workflows, root, minWorkflowFiles)
	}
	if res.Actions < minActionFiles {
		t.Fatalf("scanned only %d composite-action file(s) in %s, want at least %d: the "+
			"action.yml half of the walk is dead, which is precisely how a piped step in a "+
			"composite action stayed invisible to this guard", res.Actions, root, minActionFiles)
	}
	t.Logf("audited %d run step(s) across %d workflow(s) and %d action file(s)",
		res.Steps, res.Workflows, res.Actions)

	if len(res.Findings) > 0 {
		t.Fatalf("these run steps reach the shell WITHOUT pipefail, so a failing command in a "+
			"pipeline would be discarded and the step would report success (#521):\n  %s\n\n"+
			"Fix at the workflow level:\n\ndefaults:\n  run:\n    shell: bash -euo pipefail {0}\n\n"+
			"The bare `bash` keyword also works — GitHub documents it as "+
			"`bash --noprofile --norc -eo pipefail {0}`. A composite action takes no workflow "+
			"defaults, so its steps must say it themselves. Setting pipefail in the step body "+
			"also works, but it must come BEFORE the step's first pipeline, and not from "+
			"inside a heredoc.",
			strings.Join(res.Findings, "\n  "))
	}
}

// THE FLOOR IS FALSIFIED, not merely asserted. Handed a tree with nothing in it,
// the audit must ERROR rather than return a clean, empty result — because
// "nothing to check" and "everything checks out" are the two states this whole
// file exists to keep apart.
func TestTheShellAuditRefusesAnEmptyUniverse(t *testing.T) {
	empty := t.TempDir()

	if _, err := auditRunStepShells(empty); err == nil {
		t.Fatal("the audit accepted a tree with no CI files in it and reported no error; " +
			"an empty universe must fail, not pass vacuously")
	}

	// A .github/workflows that exists but holds nothing must fail the same way:
	// deleting the workflows is not a way to satisfy the guard.
	if err := os.MkdirAll(filepath.Join(empty, ".github", "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := auditRunStepShells(empty); err == nil {
		t.Fatal("the audit accepted an EMPTY .github/workflows and reported no error")
	}
}

// COMPOSITE ACTIONS LIVE ANYWHERE. `uses:` takes a path, so an action.yml at the
// repository root or under tools/ is as real as one under .github — and each of
// these, with a piped step and a shell that has no pipefail, left the first
// version of this guard green.
func TestTheShellAuditFindsCompositeActionsOutsideDotGithub(t *testing.T) {
	const rogue = "runs:\n  using: composite\n  steps:\n    - run: echo hi | tee log\n      shell: %s\n"

	cases := []struct {
		name  string
		path  []string
		shell string
	}{
		{name: "at the repository root", path: []string{"action.yml"}, shell: "sh"},
		{name: "under tools/", path: []string{"tools", "myaction", "action.yml"}, shell: "bash -e {0}"},
		{name: "with the .yaml spelling", path: []string{"ci", "act", "action.yaml"}, shell: "sh"},
		{name: "several directories down", path: []string{"a", "b", "c", "d", "action.yml"}, shell: "sh"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// A compliant workflow, so the ONLY thing that can produce a
			// finding is the composite action placed outside .github.
			wf := filepath.Join(dir, ".github", "workflows")
			if err := os.MkdirAll(wf, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			clean := "defaults:\n  run:\n    shell: bash\njobs:\n  a:\n    steps:\n      - run: echo ok\n"
			if err := os.WriteFile(filepath.Join(wf, "probe.yml"), []byte(clean), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			full := append([]string{dir}, c.path...)
			target := filepath.Join(full...)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			body := strings.Replace(rogue, "%s", c.shell, 1)
			if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			res, err := auditRunStepShells(dir)
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			if res.Actions == 0 {
				t.Fatalf("the audit scanned NO action files: it never saw %s at all, so its "+
					"clean result says nothing about it", filepath.Join(c.path...))
			}
			if len(res.Findings) == 0 {
				t.Fatalf("a composite action at %s pipes into tee under `shell: %s` and the "+
					"audit reported no finding; that step's pipeline is ungraded (#521)",
					filepath.Join(c.path...), c.shell)
			}
		})
	}
}

// The walk must not wander into checked-in dependency trees, which are neither
// ours to fix nor meaningful to grade.
func TestTheShellAuditSkipsVendoredTrees(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	clean := "defaults:\n  run:\n    shell: bash\njobs:\n  a:\n    steps:\n      - run: echo ok\n"
	if err := os.WriteFile(filepath.Join(wf, "probe.yml"), []byte(clean), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, skipped := range []string{"node_modules", "vendor", ".git"} {
		d := filepath.Join(dir, skipped, "pkg")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		bad := "runs:\n  using: composite\n  steps:\n    - run: echo hi | tee log\n      shell: sh\n"
		if err := os.WriteFile(filepath.Join(d, "action.yml"), []byte(bad), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	res, err := auditRunStepShells(dir)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("the audit graded a vendored tree: %v", res.Findings)
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
			// PIPEFAIL WITHOUT NOUNSET IS COMPLIANT. This guard checks the
			// property #521 was about and says so; it is not a general shell
			// hygiene gate.
			name:    "pipefail alone, no -u anywhere",
			yaml:    "defaults:\n  run:\n    shell: bash -o pipefail {0}\njobs:\n  a:\n    steps:\n      - run: echo hi | tee log\n",
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
			// A composite action does NOT inherit a caller's `defaults`, so a
			// `defaults` block in it protects nothing.
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
			res, err := auditRunStepShells(dir)
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			// The reusable-workflow case legitimately audits nothing; every
			// other case must have found a step to judge, or the audit is
			// blind rather than satisfied.
			if c.name != "a job that calls a reusable workflow has no steps to audit" && res.Steps == 0 {
				t.Fatalf("the audit saw NO run step in this spelling — it is blind to it, not clean on it:\n%s", c.yaml)
			}
			if got := len(res.Findings) > 0; got != c.wantHit {
				t.Fatalf("findings=%v (%v), want a finding=%v for:\n%s", got, res.Findings, c.wantHit, c.yaml)
			}
		})
	}
}

// PIPEFAIL HAS TO BE IN EFFECT WHEN THE PIPELINE RUNS.
//
// The first version of this guard accepted `set -euo pipefail` ANYWHERE in the
// body, which grades nothing: after the pipe it is too late, and inside a
// heredoc it is not the step's shell settings at all — it is text being written
// to some other file. Both spellings passed while the step's own pipeline stayed
// ungraded, which is #521 wearing the guard's own badge.
func TestPipefailMustPrecedeTheStepsFirstPipeline(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantGraded bool
	}{
		{
			name:       "before the pipe",
			body:       "set -euo pipefail\necho hi | tee log\n",
			wantGraded: true,
		},
		{
			name:       "the -o spelling before the pipe",
			body:       "set -o pipefail\necho hi | tee log\n",
			wantGraded: true,
		},
		{
			name:       "AFTER the pipe, on a later line",
			body:       "echo hi | tee log\nset -euo pipefail\n",
			wantGraded: false,
		},
		{
			name:       "AFTER the pipe, on the same line",
			body:       "echo hi | tee log; set -euo pipefail\n",
			wantGraded: false,
		},
		{
			name:       "before the pipe, on the same line",
			body:       "set -euo pipefail; echo hi | tee log\n",
			wantGraded: true,
		},
		{
			name:       "inside a heredoc that writes another file",
			body:       "cat > other.sh <<'EOF'\nset -euo pipefail\nEOF\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			name:       "inside an indented heredoc that writes another file",
			body:       "cat > other.sh <<-EOF\n\tset -euo pipefail\n\tEOF\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			name:       "inside an unquoted heredoc that writes another file",
			body:       "cat > other.sh <<EOF\nset -euo pipefail\nEOF\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			name:       "inside an unterminated heredoc",
			body:       "cat > other.sh <<'EOF'\nset -euo pipefail\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			name:       "merely echoed inside quotes",
			body:       "echo \"set -euo pipefail\"\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			// A real setting BEFORE a heredoc still counts; stripping heredoc
			// bodies must not swallow the step's own statements.
			name:       "set before a heredoc that is itself piped",
			body:       "set -euo pipefail\ncat <<'EOF' | tee log\nhello\nEOF\n",
			wantGraded: true,
		},
		{
			// The heredoc's OWN redirection line is executed, so a pipe on it
			// counts against a step that never set pipefail.
			name:       "a heredoc line that pipes, with no pipefail anywhere",
			body:       "cat <<'EOF' | tee log\nhello\nEOF\n",
			wantGraded: false,
		},
		{
			// `||` is not a pipeline.
			name:       "a logical or is not a pipe",
			body:       "false || true\nset -euo pipefail\n",
			wantGraded: true,
		},
		{
			// A `|` inside a quoted regex is not a pipeline either, so a later
			// `set` still arrives in time.
			name:       "a pipe inside a quoted regex is not a pipeline",
			body:       "grep -E 'a|b' f\nset -euo pipefail\n",
			wantGraded: true,
		},
		{
			// ...but the REAL pipe on such a line is still seen.
			name:       "a real pipe on a line that also has a quoted one",
			body:       "grep -E 'a|b' f | tee log\nset -euo pipefail\n",
			wantGraded: false,
		},
		{
			name:       "no pipefail at all, and no pipe either",
			body:       "echo hi\n",
			wantGraded: false,
		},
		{
			name:       "a comment mentioning it does not count",
			body:       "# set -euo pipefail\necho hi | tee log\n",
			wantGraded: false,
		},
		{
			name:       "a trailing comment mentioning it does not count",
			body:       "echo hi  # set -euo pipefail\necho hi | tee log\n",
			wantGraded: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stepIsGraded("", c.body); got != c.wantGraded {
				t.Fatalf("stepIsGraded=%v, want %v for body:\n%s", got, c.wantGraded, c.body)
			}
			// A shell that supplies pipefail outright settles it regardless of
			// where the body says anything.
			if !stepIsGraded("bash -euo pipefail {0}", c.body) {
				t.Fatal("a shell that names pipefail must grade the step whatever the body says")
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

// auditReport is what the walk saw. The file counts are reported separately from
// the step count because each is a floor in its own right: a caller cannot tell
// a clean tree from an unread one by the finding count alone.
type auditReport struct {
	Workflows int
	Actions   int
	Steps     int
	Findings  []string
}

// auditRunStepShells judges every `run:` step in the repository's workflows and
// composite actions, and returns a finding for each one that reaches the shell
// without pipefail.
//
// It errors — rather than returning an empty report — when there is nothing to
// audit at all.
func auditRunStepShells(root string) (auditReport, error) {
	var res auditReport

	workflows, actions, err := workflowAndActionFiles(root)
	if err != nil {
		return res, err
	}
	if len(workflows)+len(actions) == 0 {
		return res, &auditError{"no workflow or composite-action files found under " + root}
	}
	res.Workflows = len(workflows)
	res.Actions = len(actions)

	for _, path := range append(append([]string{}, workflows...), actions...) {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return res, rerr
		}
		var f ymlFile
		if uerr := yaml.Unmarshal(raw, &f); uerr != nil {
			return res, &auditError{path + ": " + uerr.Error()}
		}
		rel, _ := filepath.Rel(root, path)

		// A composite action does NOT inherit a caller's `defaults`, so its
		// steps are judged on their own shell alone.
		if f.Runs != nil {
			for i, s := range f.Runs.Steps {
				if strings.TrimSpace(s.Run) == "" {
					continue
				}
				res.Steps++
				if !stepIsGraded(s.Shell, s.Run) {
					res.Findings = append(res.Findings, describe(rel, "runs", i, s))
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
				res.Steps++
				// Most specific wins, exactly as GitHub resolves it.
				shell := firstNonEmpty(s.Shell, job.Defaults.Run.Shell, f.Defaults.Run.Shell)
				if !stepIsGraded(shell, s.Run) {
					res.Findings = append(res.Findings, describe(rel, id, i, s))
				}
			}
		}
	}
	return res, nil
}

type auditError struct{ msg string }

func (e *auditError) Error() string { return e.msg }

// skippedDirs are trees we neither wrote nor can fix. Everything else in the
// repository is walked, because `uses:` takes a path and a composite action is
// therefore wherever someone put it.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// workflowAndActionFiles returns the workflow files, which GitHub reads only
// from .github/workflows, and the composite-action files, which may be ANYWHERE
// in the tree.
func workflowAndActionFiles(root string) (workflows, actions []string, err error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, &auditError{root + " is not a directory"}
	}
	workflowDir := filepath.Join(root, ".github", "workflows")

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if path != root && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}
		switch base := filepath.Base(path); {
		case base == "action.yml" || base == "action.yaml":
			actions = append(actions, path)
		case filepath.Dir(path) == workflowDir:
			workflows = append(workflows, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Strings(workflows)
	sort.Strings(actions)
	return workflows, actions, nil
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
var bodyPipefail = regexp.MustCompile(`(^|;|&&|\|\|)\s*set\s+-[A-Za-z]*o[A-Za-z]*\s+pipefail`)

// stepIsGraded reports whether this step's pipelines are graded: either the
// shell it runs under enables pipefail, or the step's own body enables it BEFORE
// the step's first pipeline.
//
// Position is the whole point. `foo | bar` followed by `set -o pipefail` is an
// ungraded pipeline and a setting nothing uses, and it used to satisfy this
// check. So does a `set -o pipefail` inside a heredoc, which is not a shell
// setting at all — it is a line of text on its way into another file.
func stepIsGraded(shell, body string) bool {
	if shellEnablesPipefail(shell) {
		return true
	}
	for _, line := range strings.Split(executedBody(body), "\n") {
		masked := maskInertText(line)
		pf := -1
		if loc := bodyPipefail.FindStringIndex(masked); loc != nil {
			pf = loc[0]
		}
		pipe := firstPipeCol(masked)
		switch {
		case pf >= 0 && (pipe < 0 || pf < pipe):
			// pipefail is in effect from here on, and no pipeline has run yet.
			return true
		case pipe >= 0:
			// A pipeline ran before anything set pipefail: its exit status is
			// already lost, whatever the rest of the body says.
			return false
		}
	}
	return false
}

// heredocStart matches a heredoc redirection and captures its delimiter. `<<<`
// is a here-STRING, not a heredoc, and is excluded.
var heredocStart = regexp.MustCompile(`<<(-?)(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))`)

// executedBody strips heredoc CONTENT, keeping the redirection line itself,
// which really is executed and really can contain a pipe.
//
// An unterminated heredoc swallows the rest of the body. That is the
// conservative direction: the alternative is crediting the step for settings
// that were never executed.
func executedBody(body string) string {
	var out []string
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		out = append(out, line)

		m := heredocStart.FindStringSubmatch(line)
		if m == nil || strings.Contains(line, "<<<") {
			continue
		}
		delim := m[2] + m[3] + m[4]
		if delim == "" {
			continue
		}
		dashed := m[1] == "-"
		for i+1 < len(lines) {
			i++
			candidate := lines[i]
			if dashed {
				candidate = strings.TrimLeft(candidate, " \t")
			}
			if candidate == delim {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// maskInertText blanks out quoted strings and trailing comments, preserving
// every column so that positions found in the result are positions in the
// original. `echo "set -o pipefail"` is not a setting, and `grep 'a|b'` is not a
// pipeline; neither may be read as one.
func maskInertText(line string) string {
	out := []byte(line)
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			out[i] = ' '
		case c == '\\':
			if i+1 < len(out) {
				out[i] = ' '
				out[i+1] = ' '
				i++
			}
		case c == '\'' || c == '"':
			quote = c
			out[i] = ' '
		case c == '#' && (i == 0 || out[i-1] == ' ' || out[i-1] == '\t' || out[i-1] == ';'):
			for ; i < len(out); i++ {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

// firstPipeCol returns the column of the first pipeline operator, or -1. `||` is
// a logical or, not a pipe.
func firstPipeCol(masked string) int {
	for i := 0; i < len(masked); i++ {
		if masked[i] != '|' {
			continue
		}
		if i+1 < len(masked) && masked[i+1] == '|' {
			i++
			continue
		}
		if i > 0 && masked[i-1] == '|' {
			continue
		}
		return i
	}
	return -1
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
