package repomigration

import (
	"context"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// GitHistoryPusher is the seam where the DEFERRED git history/refs push will
// plug in. After the orchestrator provisions an (empty) target repository over
// REST, it calls Push to transfer the source repository's commits, branches, and
// tags into the new target repository.
//
// This is intentionally NOT implemented in this REST slice. The real mechanism
// is non-REST — clone the source repo and push to the target via git CLI or
// go-git — and carries its own authentication design (a Git credential, not the
// ADO REST bearer token). See the package doc and the PR's "Deferred" note. The
// orchestrator defaults to noopGitHistoryPusher, so repos are provisioned empty
// until a real pusher is injected in a later slice.
type GitHistoryPusher interface {
	// Push transfers git history from the source repository into the freshly
	// created target repository. Implementations must be idempotent (a re-push of
	// already-present refs is a no-op).
	Push(ctx context.Context, source, target ado.Repository) error
}

// noopGitHistoryPusher is the default pusher: it performs no transfer, leaving
// target repositories empty. It exists so the orchestrator has a stable seam and
// can run end-to-end (provisioning only) before the git-history slice lands.
type noopGitHistoryPusher struct{}

// Push does nothing and never fails.
func (noopGitHistoryPusher) Push(ctx context.Context, source, target ado.Repository) error {
	return nil
}
