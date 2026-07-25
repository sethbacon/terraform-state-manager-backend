package statesource

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// gitConn reads state from a Git repository by shallow-cloning the configured
// ref into memory; writes commit to that ref and push (never force). Auth is a
// token (used as the HTTP basic password; username defaults to "git").
type gitConn struct {
	repoURL     string
	ref         string
	prefix      string
	username    string
	token       string
	authorName  string
	authorEmail string
}

// gitLog tags mutation logs from this connector.
var gitLog = slog.With("component", "statesource.git")

func newGit(config, creds map[string]any) (*gitConn, error) {
	repoURL, _ := config["repo_url"].(string)
	if repoURL == "" {
		return nil, fmt.Errorf("git source requires config.repo_url")
	}
	ref, _ := config["ref"].(string)
	prefix, _ := config["prefix"].(string)
	username, _ := config["username"].(string)
	if username == "" {
		username = "git"
	}
	authorName, _ := config["author_name"].(string)
	if authorName == "" {
		authorName = "Terraform State Manager"
	}
	authorEmail, _ := config["author_email"].(string)
	if authorEmail == "" {
		authorEmail = "tsm@noreply.local"
	}
	token, _ := creds["token"].(string)
	return &gitConn{
		repoURL: repoURL, ref: ref, prefix: prefix, username: username, token: token,
		authorName: authorName, authorEmail: authorEmail,
	}, nil
}

func (g *gitConn) checkout(ctx context.Context) (billy.Filesystem, error) {
	fs := memfs.New()
	opts := &git.CloneOptions{
		URL:          g.repoURL,
		Depth:        1,
		SingleBranch: true,
		Tags:         git.NoTags,
	}
	if g.ref != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(g.ref)
	}
	if g.token != "" {
		opts.Auth = &githttp.BasicAuth{Username: g.username, Password: g.token}
	}
	if _, err := git.CloneContext(ctx, memory.NewStorage(), fs, opts); err != nil {
		return nil, fmt.Errorf("git clone failed: %w", err)
	}
	return fs, nil
}

func (g *gitConn) List(ctx context.Context) ([]StateRef, error) {
	fs, err := g.checkout(ctx)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimPrefix(g.prefix, "/")
	var refs []StateRef
	_ = billyutil.Walk(fs, "/", func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		// billyutil.Walk joins path segments with filepath.Join, which uses
		// OS-native separators even for this in-memory filesystem; normalize
		// to "/" so prefix/suffix matching below is platform-independent.
		rel := strings.TrimPrefix(filepath.ToSlash(p), "/")
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			return nil
		}
		if !strings.HasSuffix(rel, ".tfstate") {
			return nil
		}
		mod := info.ModTime()
		refs = append(refs, StateRef{Key: rel, Name: rel, Size: info.Size(), LastModified: &mod})
		return nil
	})
	return refs, nil
}

func (g *gitConn) Read(ctx context.Context, key string) (*RawState, error) {
	fs, err := g.checkout(ctx)
	if err != nil {
		return nil, err
	}
	f, err := fs.Open("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		return nil, fmt.Errorf("failed to read %q from git: %w", key, err)
	}
	defer func() { _ = f.Close() }()
	data, err := readCapped(f)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

func (g *gitConn) auth() *githttp.BasicAuth {
	if g.token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: g.username, Password: g.token}
}

// Write commits the state to the configured ref and pushes. Guards: keys must
// be clean .tfstate paths inside the repo; identical content is a no-op (no
// empty commits); pushes are never forced, so a rejected push (non-fast-
// forward, protected branch) surfaces verbatim for the caller to handle.
func (g *gitConn) Write(ctx context.Context, key string, data []byte) error {
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return fmt.Errorf("invalid state key %q (path traversal)", key)
		}
	}
	rel := strings.TrimPrefix(path.Clean("/"+key), "/")
	if rel == "" || rel == "." {
		return fmt.Errorf("invalid state key %q", key)
	}
	if !strings.HasSuffix(rel, ".tfstate") {
		return fmt.Errorf("git state keys must end in .tfstate (got %q)", key)
	}

	// Full in-memory clone: pushing from a shallow clone is unreliable, and
	// the commit needs real history to fast-forward onto.
	fs := memfs.New()
	opts := &git.CloneOptions{URL: g.repoURL, SingleBranch: true, Tags: git.NoTags}
	if g.ref != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(g.ref)
	}
	if a := g.auth(); a != nil {
		opts.Auth = a
	}
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, opts)
	if err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("git worktree failed: %w", err)
	}

	if err := billyutil.WriteFile(fs, rel, data, 0o600); err != nil {
		return fmt.Errorf("failed to stage %q: %w", key, err)
	}
	if _, err := wt.Add(rel); err != nil {
		return fmt.Errorf("git add failed: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("git status failed: %w", err)
	}
	if status.IsClean() {
		gitLog.Info("state unchanged, skipping commit", "repo", g.repoURL, "key", rel)
		return nil
	}

	commit, err := wt.Commit(fmt.Sprintf("tsm: write state %s", rel), &git.CommitOptions{
		Author: &object.Signature{Name: g.authorName, Email: g.authorEmail, When: time.Now()},
	})
	if err != nil {
		return fmt.Errorf("git commit failed: %w", err)
	}

	pushOpts := &git.PushOptions{}
	if a := g.auth(); a != nil {
		pushOpts.Auth = a
	}
	if g.ref != "" {
		ref := plumbing.NewBranchReferenceName(g.ref)
		pushOpts.RefSpecs = []config.RefSpec{config.RefSpec(ref + ":" + ref)}
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil {
		return fmt.Errorf("git push failed (the branch may be protected or have advanced — retry): %w", err)
	}
	gitLog.Info("state committed and pushed",
		"repo", g.repoURL, "ref", g.ref, "key", rel, "commit", commit.String(), "bytes", len(data))
	return nil
}

// Delete removes the state file at key with a deletion commit pushed to the
// configured ref (never forced). A missing file is reported as ErrNotFound.
func (g *gitConn) Delete(ctx context.Context, key string) error {
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return fmt.Errorf("invalid state key %q (path traversal)", key)
		}
	}
	rel := strings.TrimPrefix(path.Clean("/"+key), "/")
	if rel == "" || rel == "." {
		return fmt.Errorf("invalid state key %q", key)
	}

	fs := memfs.New()
	opts := &git.CloneOptions{URL: g.repoURL, SingleBranch: true, Tags: git.NoTags}
	if g.ref != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(g.ref)
	}
	if a := g.auth(); a != nil {
		opts.Auth = a
	}
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, opts)
	if err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("git worktree failed: %w", err)
	}
	if _, err := fs.Stat(rel); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("state %q %w", key, ErrNotFound)
		}
		return fmt.Errorf("failed to stat %q: %w", key, err)
	}
	if _, err := wt.Remove(rel); err != nil {
		return fmt.Errorf("git rm failed: %w", err)
	}
	commit, err := wt.Commit(fmt.Sprintf("tsm: delete state %s", rel), &git.CommitOptions{
		Author: &object.Signature{Name: g.authorName, Email: g.authorEmail, When: time.Now()},
	})
	if err != nil {
		return fmt.Errorf("git commit failed: %w", err)
	}
	pushOpts := &git.PushOptions{}
	if a := g.auth(); a != nil {
		pushOpts.Auth = a
	}
	if g.ref != "" {
		ref := plumbing.NewBranchReferenceName(g.ref)
		pushOpts.RefSpecs = []config.RefSpec{config.RefSpec(ref + ":" + ref)}
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil {
		return fmt.Errorf("git push failed (the branch may be protected or have advanced — retry): %w", err)
	}
	gitLog.Info("state deleted and pushed",
		"repo", g.repoURL, "ref", g.ref, "key", rel, "commit", commit.String())
	return nil
}
