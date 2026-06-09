package repomigration

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// GitHistoryPusher is the seam through which an EXECUTE run transfers a source
// repository's git history into the freshly-provisioned target repository. After
// the orchestrator creates an (empty) target repo over REST, it calls Push once
// per repository to migrate the commits, branches, and tags.
//
// The default remains noopGitHistoryPusher (provision empty); GoGitPusher is the
// real, pure-Go implementation (clone source → push to target). Selecting the
// real pusher is the caller's choice when a source URL and credentials are
// available — see NewService.
type GitHistoryPusher interface {
	// Push transfers git history from the source repository into the freshly
	// created target repository. Implementations must be idempotent (a re-push of
	// already-present refs is a no-op).
	Push(ctx context.Context, source, target ado.Repository) error
}

// noopGitHistoryPusher is the default pusher: it performs no transfer, leaving
// target repositories empty. It exists so the orchestrator has a stable seam and
// can run end-to-end (provisioning only) when no source URL or auth is available.
type noopGitHistoryPusher struct{}

// Push does nothing and never fails.
func (noopGitHistoryPusher) Push(ctx context.Context, source, target ado.Repository) error {
	return nil
}

// GitAuthProvider supplies the transport credentials used to clone the source
// repository and push to the target. It is an injectable seam so the live
// ADO-token auth (HTTP basic with a WIF-derived bearer, mirroring the
// federation.TokenProvider pattern) can plug in later without touching the
// pusher. The argument is the remote URL being authenticated so a provider may
// return different credentials for the source vs. target endpoints.
//
// Returning (nil, nil) means "no auth" — used by tests against local file://
// repositories. A non-nil error aborts the operation.
type GitAuthProvider interface {
	// AuthFor returns the transport AuthMethod for remoteURL, or nil for no auth.
	AuthFor(ctx context.Context, remoteURL string) (transport.AuthMethod, error)
}

// noAuthProvider returns no credentials for any remote. It is the default for
// local-fixture tests and any endpoint that does not require authentication.
type noAuthProvider struct{}

// AuthFor always returns (nil, nil): no transport authentication.
func (noAuthProvider) AuthFor(_ context.Context, _ string) (transport.AuthMethod, error) {
	return nil, nil
}

// NoAuth is a shared no-credential GitAuthProvider for callers and tests that
// push to endpoints requiring no authentication (e.g. local file:// fixtures).
//
// TODO(repomigration): add a live ADO GitAuthProvider that wraps a
// federation.TokenProvider and returns an HTTP basic AuthMethod
// (&githttp.BasicAuth{Username: "tsm", Password: <wif-derived bearer>}) so
// history is pushed to Azure DevOps with the same WIF credentials the REST
// orchestrator already uses. Deferred with the rest of the "WIF later" work.
var NoAuth GitAuthProvider = noAuthProvider{}

// GoGitPusher is a pure-Go GitHistoryPusher built on go-git. It clones every ref
// (all branches and tags) from the source repository into an in-memory store and
// pushes them to the target repository over the configured transport. It uses no
// git binary and no cgo.
//
// Idempotency: a push of refs already present on the target is reported by go-git
// as transport.ErrEmptyUploadPackRequest / git.NoErrAlreadyUpToDate, both of
// which Push treats as success. A resumed migration may therefore re-run the git
// step safely.
type GoGitPusher struct {
	auth GitAuthProvider
}

// NewGoGitPusher constructs a GoGitPusher. If auth is nil the pusher uses NoAuth
// (no credentials) — appropriate for local fixtures and for early wiring before
// the live ADO Git auth lands.
func NewGoGitPusher(auth GitAuthProvider) *GoGitPusher {
	if auth == nil {
		auth = NoAuth
	}
	return &GoGitPusher{auth: auth}
}

// refSpecAllHeadsTags mirrors every branch and tag from source to target. The
// leading "+" forces non-fast-forward updates so a re-push reconciles cleanly;
// because each ref maps to an identical name, an already-present ref is a no-op.
var refSpecAllHeadsTags = []config.RefSpec{
	"+refs/heads/*:refs/heads/*",
	"+refs/tags/*:refs/tags/*",
}

// Push clones all refs from source.RemoteURL and pushes them to
// target.RemoteURL. Both URLs are taken from the ADO repository records (the
// source is the repo being migrated; the target is the freshly created empty
// repo). A missing source URL is treated as "nothing to migrate" and returns nil
// so the orchestrator can fall back to provisioning empty repositories.
func (p *GoGitPusher) Push(ctx context.Context, source, target ado.Repository) error {
	if source.RemoteURL == "" {
		// No source to clone from: nothing to migrate, provision empty.
		return nil
	}
	if target.RemoteURL == "" {
		return fmt.Errorf("git history push: target repository %q has no remote URL", target.Name)
	}

	srcAuth, err := p.auth.AuthFor(ctx, source.RemoteURL)
	if err != nil {
		return fmt.Errorf("git history push: source auth for %q: %w", source.Name, err)
	}
	dstAuth, err := p.auth.AuthFor(ctx, target.RemoteURL)
	if err != nil {
		return fmt.Errorf("git history push: target auth for %q: %w", target.Name, err)
	}

	// Mirror-clone the source into an in-memory repository so no working tree or
	// temp directory is required. Mirror brings every ref (branches + tags).
	repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:    source.RemoteURL,
		Mirror: true,
		Auth:   srcAuth,
	})
	if err != nil {
		return fmt.Errorf("git history push: cloning source %q: %w", source.Name, err)
	}

	// Point a target remote at the destination and mirror all heads and tags.
	const remoteName = "tsm-target"
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: remoteName,
		URLs: []string{target.RemoteURL},
	}); err != nil {
		return fmt.Errorf("git history push: configuring target remote for %q: %w", target.Name, err)
	}

	pushOpts := &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   refSpecAllHeadsTags,
		Auth:       dstAuth,
		Force:      true,
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil {
		if isAlreadyUpToDate(err) {
			// Idempotent: every ref is already present on the target.
			return nil
		}
		return fmt.Errorf("git history push: pushing to target %q: %w", target.Name, err)
	}
	return nil
}

// isAlreadyUpToDate reports whether err signals that the target already holds the
// refs being pushed — the idempotent re-push case, which is not a failure.
// go-git surfaces this as git.NoErrAlreadyUpToDate, and a mirror push of an
// up-to-date remote may additionally yield transport.ErrEmptyUploadPackRequest.
func isAlreadyUpToDate(err error) bool {
	return errors.Is(err, git.NoErrAlreadyUpToDate) ||
		errors.Is(err, transport.ErrEmptyUploadPackRequest)
}
