package statesource

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/storage/memory"
)

// gitConn reads state from a Git repository by shallow-cloning the configured ref
// into memory. Writing (commit/push) is not supported yet. Auth is a token (used
// as the HTTP basic password; username defaults to "git").
type gitConn struct {
	repoURL  string
	ref      string
	prefix   string
	username string
	token    string
}

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
	token, _ := creds["token"].(string)
	return &gitConn{repoURL: repoURL, ref: ref, prefix: prefix, username: username, token: token}, nil
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
		rel := strings.TrimPrefix(p, "/")
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
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// Write is not supported yet: committing and pushing edited state back to a Git
// repo (with the right author, branch, and conflict handling) is a follow-up.
func (g *gitConn) Write(_ context.Context, _ string, _ []byte) error {
	return fmt.Errorf("writing state to a git source is not supported yet")
}
