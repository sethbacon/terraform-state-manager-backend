// notification_channel_scope.go is the Phase 3 read flip for notification_channels
// (#393, tracking #502) — the eighth of nine partition roots.
//
// # What was already closed, and what this closes
//
// The DELIVERY path was scoped first and is not touched here: identity/notify
// exposes WithOrgScope as a channel query option, Notifier.Notify forwards its
// variadic options to ListEnabledForEvent, and this application passes
// notify.ForOrganization at all three Notify call sites
// (internal/services/notify/scope.go). An event raised inside one organization
// does not fan out to another organization's webhook.
//
// What was still open is the CRUD surface an operator drives. ListChannels read
// every organization's channels; UpdateChannel, DeleteChannel and the test-send
// found their row BY ID ALONE. So all three sides of the partition were open on
// this root at once — read, mutate, and destroy — and none of them needed a
// library change to close, only a call site that said which organization it was
// acting for.
//
// # Why this root's by-id read is the one that hurts
//
// A channel's encrypted_target is a CAPABILITY-BEARING SECRET (000009:8): a
// Slack or Teams incoming-webhook URL, a generic webhook endpoint, or an email
// recipient list. ChannelRepository.List blanks it, but GetByID returns it, and
// the test-send decrypts it and POSTs to it. So an unscoped by-id read on this
// table is not "an operator sees a row they should not" — it is one tenant
// holding another tenant's webhook credential, and an unscoped test-send is one
// tenant making the deployment POST to it. That is why the by-id twin below
// exists even though this application exposes no GET /channels/{id} route: the
// route that reaches the secret is POST /channels/{id}/test.
//
// # The predicate excludes NULL, like every other root
//
// store.OrgScopeOrganizations renders `organization_id = ANY($n)`, and
// `NULL = ANY(...)` is NULL rather than true. 000034 made the column NOT NULL,
// but a database restored from an older backup still holds unstamped rows: those
// are invisible to every tenant rather than visible to all of them, and reachable
// only by a platform admin — which is what keeps them repairable instead of lost.
package repositories

import (
	"context"
	"errors"
	"strings"

	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ChannelOrgScope is the ONE conversion from TSM organization ids to
// identity/notify's channel query scope. Both consumers go through it: the
// InScope readers below, and internal/services/notify.ForOrganization on the
// delivery path.
//
// It is here rather than beside ForOrganization because internal/services/notify
// imports this package and not the other way round, and because a conversion
// written twice is a conversion that will be written permissively once.
//
// BLANK IDS ARE DROPPED, not passed through. An empty string bound into
// `organization_id = ANY($1)` against a uuid column is an error at best; where
// the cast succeeds it is a predicate matching a row nobody owns. And an id list
// that empties out yields OrgScopeOrganizations() — which renders the literal
// FALSE — so the fail-closed direction is what falls out of the blank case
// rather than something a caller has to remember.
func ChannelOrgScope(organizationIDs ...string) idstore.OrgScope {
	ids := make([]string, 0, len(organizationIDs))
	for _, id := range organizationIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return idstore.OrgScopeOrganizations(ids...)
}

// channelScopeOption is the query option for a non-empty, non-platform-admin
// scope. The two other cases are decided by the callers below, before this is
// reached, so this never has to encode "reach everything".
func channelScopeOption(scope tenantscope.Scope) identitynotify.ChannelQueryOption {
	return identitynotify.WithOrgScope(ChannelOrgScope(scope.OrgIDs...))
}

// ListInScope returns the channels the scope permits, without their targets.
//
// An empty scope reads NOTHING, without a query. The early return is not an
// optimisation: it states the fail-closed answer where a later edit cannot
// change it by accident. Here it also removes a dependence on the shared
// library's rendering of an empty id list — that is currently the literal FALSE,
// which is exactly right, and it is a fact about somebody else's package.
func (r *NotificationChannelRepository) ListInScope(ctx context.Context, scope tenantscope.Scope) ([]NotificationChannel, error) {
	if scope.Empty() {
		return []NotificationChannel{}, nil
	}
	if scope.PlatformAdmin {
		return r.List(ctx)
	}
	return r.inner.List(ctx, channelScopeOption(scope))
}

// GetByIDInScope returns one channel, WITH its encrypted target, when the scope
// permits it — and (nil, nil) otherwise.
//
// A channel in another organization is reported EXACTLY as one that does not
// exist, and here that is not only about id enumeration: the alternative answer
// confirms to a caller that a webhook they cannot read exists, on a row whose
// whole content is a credential.
//
// (nil, nil) rather than the shared package's ErrNotFound because that is this
// repository's by-id convention (SourceRepository, ScheduleRepository, all three
// callback roots) and a caller must not have to remember which of two spellings
// of "no row" a given reader speaks.
func (r *NotificationChannelRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*NotificationChannel, error) {
	if scope.Empty() {
		return nil, nil
	}
	var (
		ch  *NotificationChannel
		err error
	)
	if scope.PlatformAdmin {
		ch, err = r.GetByID(ctx, id)
	} else {
		ch, err = r.inner.GetByID(ctx, id, channelScopeOption(scope))
	}
	if errors.Is(err, idstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// UpdateInScope replaces a channel's mutable fields only when the caller's
// organization owns it.
//
// The mutating side is scoped in the SAME change as the read, because a boundary
// a list read enforces and a by-id mutation does not is not a boundary. On this
// table the mutation is also a write to a secret: PUT /channels/{id} with a
// target re-seals a new destination into the row, so an unscoped update lets one
// tenant redirect another tenant's alerts at an endpoint of their choosing.
//
// A row outside the scope matches nothing and is reported with identity/store's
// ErrNotFound, which the handler renders as 404 — the same answer a channel that
// does not exist gets.
func (r *NotificationChannelRepository) UpdateInScope(ctx context.Context, id, name, typ string, events []string, enabled bool, encryptedTarget string, scope tenantscope.Scope) (*NotificationChannel, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return nil, ErrNotInScope
	}
	if w.Skip {
		return r.Update(ctx, id, name, typ, events, enabled, encryptedTarget)
	}
	return r.inner.Update(ctx, id, name, typ, events, enabled, encryptedTarget,
		channelScopeOption(scope))
}

// DeleteInScope removes a channel only when the caller's organization owns it.
//
// Returns identity/store's ErrNotFound when nothing matched, covering both "no
// such id" and "that id belongs to another organization". The handler treats an
// already-absent channel as 204 either way, which is the pre-existing idempotent
// contract and is also the non-disclosing one.
func (r *NotificationChannelRepository) DeleteInScope(ctx context.Context, id string, scope tenantscope.Scope) error {
	w := scopeWrite(scope)
	if w.Deny {
		return ErrNotInScope
	}
	if w.Skip {
		return r.Delete(ctx, id)
	}
	return r.inner.Delete(ctx, id, channelScopeOption(scope))
}
