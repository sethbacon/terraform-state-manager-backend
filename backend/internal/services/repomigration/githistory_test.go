package repomigration

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// These tests exercise GoGitPusher entirely against LOCAL fixture repositories
// built in temp directories with go-git itself — no network, no credentials, no
// git binary. The "remote URL" of each fixture is its filesystem path: go-git's
// transport treats a scheme-less absolute path as a file:// endpoint (see
// transport.parseFile), which is more robust on Windows than constructing a
// file://C:\... URL by hand.

// fixtureCommit is one commit to lay down in a source fixture repository.
type fixtureCommit struct {
	file    string
	content string
	message string
}

// buildSourceRepo creates a non-bare working repository at a fresh temp path,
// lays down two commits on the default branch, creates a second branch, and a
// lightweight tag pointing at the first commit. It returns the repo path and the
// set of reference names it created so tests can assert they were mirrored.
func buildSourceRepo(t *testing.T) (path string, branch, tag string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("source worktree: %v", err)
	}

	commits := []fixtureCommit{
		{file: "README.md", content: "v1\n", message: "initial commit"},
		{file: "main.tf", content: "resource \"null_resource\" \"x\" {}\n", message: "add config"},
	}
	var firstHash plumbing.Hash
	for i, c := range commits {
		if err := writeWorktreeFile(wt, c.file, c.content); err != nil {
			t.Fatalf("write %s: %v", c.file, err)
		}
		if _, err := wt.Add(c.file); err != nil {
			t.Fatalf("add %s: %v", c.file, err)
		}
		h, err := wt.Commit(c.message, &git.CommitOptions{
			Author: fixtureSignature(),
		})
		if err != nil {
			t.Fatalf("commit %s: %v", c.message, err)
		}
		if i == 0 {
			firstHash = h
		}
	}

	// A second branch off HEAD.
	branch = "feature/widget"
	headRef, err := repo.Head()
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), headRef.Hash())
	if err := repo.Storer.SetReference(branchRef); err != nil {
		t.Fatalf("create branch %s: %v", branch, err)
	}

	// A lightweight tag on the first commit.
	tag = "v0.1.0"
	if _, err := repo.CreateTag(tag, firstHash, nil); err != nil {
		t.Fatalf("create tag %s: %v", tag, err)
	}

	return dir, branch, tag
}

// fixtureSignature returns a deterministic author/committer signature.
func fixtureSignature() *object.Signature {
	return &object.Signature{
		Name:  "Fixture Bot",
		Email: "fixture@example.com",
		When:  time.Unix(1700000000, 0).UTC(),
	}
}

// writeWorktreeFile writes content to name inside the worktree's billy filesystem.
func writeWorktreeFile(wt *git.Worktree, name, content string) error {
	f, err := wt.Filesystem.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// buildBareTarget creates an empty bare repository at a fresh temp path and
// returns the path (used as the target "remote URL").
func buildBareTarget(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare target: %v", err)
	}
	return dir
}

// openRefs opens a repository at path and returns a name→hash map of every
// branch and tag it contains.
func openRefs(t *testing.T, path string) map[string]plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	iter, err := repo.References()
	if err != nil {
		t.Fatalf("references %s: %v", path, err)
	}
	refs := map[string]plumbing.Hash{}
	_ = iter.ForEach(func(r *plumbing.Reference) error {
		if r.Type() == plumbing.HashReference {
			n := r.Name()
			if n.IsBranch() || n.IsTag() {
				refs[n.String()] = r.Hash()
			}
		}
		return nil
	})
	return refs
}

// TestGoGitPusher_PushesAllRefs builds a source fixture and an empty bare target,
// pushes source→target with no auth, and asserts the target ends up with the
// same branches, tags, and commit hashes as the source.
func TestGoGitPusher_PushesAllRefs(t *testing.T) {
	srcPath, branch, tag := buildSourceRepo(t)
	dstPath := buildBareTarget(t)

	source := ado.Repository{Name: "platform-infra", RemoteURL: srcPath}
	target := ado.Repository{ID: "tgt-id", Name: "platform-infra", RemoteURL: dstPath}

	pusher := NewGoGitPusher(nil) // nil → NoAuth
	if err := pusher.Push(context.Background(), source, target); err != nil {
		t.Fatalf("Push: %v", err)
	}

	srcRefs := openRefs(t, srcPath)
	dstRefs := openRefs(t, dstPath)

	wantBranch := plumbing.NewBranchReferenceName(branch).String()
	wantTag := plumbing.NewTagReferenceName(tag).String()
	for _, name := range []string{wantBranch, wantTag} {
		if _, ok := dstRefs[name]; !ok {
			t.Errorf("target missing ref %q (have %v)", name, keys(dstRefs))
		}
	}
	// Every source branch/tag must be present on the target with the same hash.
	for name, hash := range srcRefs {
		got, ok := dstRefs[name]
		if !ok {
			t.Errorf("target missing source ref %q", name)
			continue
		}
		if got != hash {
			t.Errorf("ref %q hash = %s, want %s", name, got, hash)
		}
	}
}

// TestGoGitPusher_RePushIsNoOp verifies idempotency: pushing a second time into a
// target that already holds every ref succeeds without error and leaves the refs
// unchanged.
func TestGoGitPusher_RePushIsNoOp(t *testing.T) {
	srcPath, _, _ := buildSourceRepo(t)
	dstPath := buildBareTarget(t)

	source := ado.Repository{Name: "platform-infra", RemoteURL: srcPath}
	target := ado.Repository{ID: "tgt-id", Name: "platform-infra", RemoteURL: dstPath}

	pusher := NewGoGitPusher(nil)
	if err := pusher.Push(context.Background(), source, target); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	refsAfterFirst := openRefs(t, dstPath)

	// Second push must be a no-op (already up to date), not an error.
	if err := pusher.Push(context.Background(), source, target); err != nil {
		t.Fatalf("second Push (should be idempotent no-op): %v", err)
	}
	refsAfterSecond := openRefs(t, dstPath)

	if len(refsAfterFirst) != len(refsAfterSecond) {
		t.Fatalf("ref count changed after re-push: %d → %d", len(refsAfterFirst), len(refsAfterSecond))
	}
	for name, hash := range refsAfterFirst {
		if refsAfterSecond[name] != hash {
			t.Errorf("ref %q changed after re-push: %s → %s", name, hash, refsAfterSecond[name])
		}
	}
}

// TestGoGitPusher_NoSourceURLIsNoOp verifies that a source with no remote URL is
// treated as "nothing to migrate" and returns nil (lets the orchestrator fall
// back to provisioning empty repositories).
func TestGoGitPusher_NoSourceURLIsNoOp(t *testing.T) {
	pusher := NewGoGitPusher(nil)
	err := pusher.Push(context.Background(),
		ado.Repository{Name: "empty"},
		ado.Repository{Name: "empty", RemoteURL: buildBareTarget(t)})
	if err != nil {
		t.Fatalf("Push with no source URL should be a no-op, got: %v", err)
	}
}

// TestGoGitPusher_MissingTargetURLErrors verifies a present source but missing
// target URL is a hard error (misconfiguration, not an idempotent skip).
func TestGoGitPusher_MissingTargetURLErrors(t *testing.T) {
	srcPath, _, _ := buildSourceRepo(t)
	pusher := NewGoGitPusher(nil)
	err := pusher.Push(context.Background(),
		ado.Repository{Name: "platform-infra", RemoteURL: srcPath},
		ado.Repository{Name: "platform-infra"})
	if err == nil {
		t.Fatal("expected error for target with no remote URL")
	}
}

// TestExecute_GitHistoryStepRecorded wires a real GoGitPusher into the
// orchestrator and verifies the git_history step is counted and recorded as
// created for the one repository in the plan, end-to-end against local fixtures.
func TestExecute_GitHistoryStepRecorded(t *testing.T) {
	srcPath, _, _ := buildSourceRepo(t)
	dstPath := buildBareTarget(t)

	store := newFakeStore(newMigration())
	// fakeTarget returns a Repository whose RemoteURL points at our bare fixture
	// so the real pusher has a place to push to.
	target := &fakeTarget{repoRemoteURL: dstPath}
	plan := &ado.MigrationPlan{
		Repositories: []ado.Repository{{ID: "src1", Name: "platform-infra", RemoteURL: srcPath}},
	}

	svc := NewService(store, NewGoGitPusher(nil), nil)
	sum, err := svc.Execute(context.Background(), "mig-1", plan, target)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// One repository + one git_history step, both created.
	if sum.Total != 2 {
		t.Errorf("total = %d, want 2 (repo + git_history)", sum.Total)
	}
	if sum.Created != 2 {
		t.Errorf("created = %d, want 2", sum.Created)
	}
	if store.stepStatus(models.RepoMigrationResourceGitHistory, "platform-infra") != models.RepoMigrationStepCreated {
		t.Error("git_history step not recorded as created")
	}

	// The history actually landed on the target.
	if len(openRefs(t, dstPath)) == 0 {
		t.Error("target has no refs after execute git-history push")
	}
}

// keys returns the key set of a ref map for error messages.
func keys(m map[string]plumbing.Hash) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
