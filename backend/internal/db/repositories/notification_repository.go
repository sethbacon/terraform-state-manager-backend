// notification_repository.go owns this application's view of the
// notification_channels DAO from the shared identity/notify package: admin-
// configured delivery destinations (webhook, Slack, Teams, or an ad-hoc email
// recipient list) for drift_detected / run_failed alerts. Requires the
// notification_channels table to be on the shared schema (encrypted_target
// TEXT, events JSONB) — see migration 000023.
//
// # Why this is a WRAPPER and no longer a type alias
//
// It was `type NotificationChannelRepository = identitynotify.ChannelRepository`,
// which cost nothing and hid something. The shared DAO expresses tenancy as an
// OPTIONAL variadic option (`notify.WithOrgScope`) because it serves consumers
// that do not partition this table at all, so on that type an unscoped read and
// a scoped one differ by an argument nobody has to pass. This repository's Phase
// 3 convention is the opposite and deliberately so: a scoped reader is a
// SEPARATE METHOD whose name ends in InScope, and internal/api's
// unscoped_twin_class_test.go derives the universe of such twins by parsing THIS
// PACKAGE. A method declared in the module cache is not in that universe, so
// while this was an alias the entire notification-channel CRUD surface was
// invisible to the guard that exists to catch exactly this.
//
// Wrapping it puts the four reads back under the guard, and gives the scope
// conversion one home (see notification_channel_scope.go) instead of one per
// call site.
package repositories

import (
	"context"
	"database/sql"

	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
)

// NotificationChannel is a destination for alerts. The target is held
// encrypted (EncryptedTarget) and never serialized; HasTarget reports whether
// one is set so the UI can show "configured" without exposing the secret.
// Note: ID is a plain string and EncryptedTarget is a plain string (base64,
// via the shared TokenCipher) — this repo's prior BYTEA/[]byte target has
// been migrated to TEXT (see migration 000023).
type NotificationChannel = identitynotify.NotificationChannel

// NotificationChannelRepository is this application's DAO for
// notification_channels, over the shared identity/notify implementation.
type NotificationChannelRepository struct {
	inner *identitynotify.ChannelRepository
}

// NewNotificationChannelRepository constructs the repository over the app connection.
func NewNotificationChannelRepository(db *sql.DB) *NotificationChannelRepository {
	return &NotificationChannelRepository{inner: identitynotify.NewChannelRepository(db)}
}

// Channels exposes the shared DAO for the one consumer that needs the shared
// TYPE rather than this application's surface: identitynotify.NewNotifier, which
// takes the concrete *ChannelRepository.
//
// It is deliberately not a general escape hatch. The notifier's own reads are
// scoped at the SEND path by the caller passing notify.ForOrganization to
// Notify — see internal/services/notify/scope.go — so handing it the shared DAO
// does not hand it an unscoped delivery path. Anything else that reaches for
// this is reaching past the InScope readers below, and should not.
//
// Nil-safe on the receiver, because notify.New accepts a nil repository (its own
// test asserts that constructing a Notifier without one does not panic) and the
// alias this replaced propagated that nil for free. A wrapper that dereferenced
// here would turn a supported "notifications unavailable" wiring into a crash at
// startup on a deployment with no encryption key.
func (r *NotificationChannelRepository) Channels() *identitynotify.ChannelRepository {
	if r == nil {
		return nil
	}
	return r.inner
}

// Create registers a channel. The owning organization travels as
// identitynotify.WithOwningOrganization; without it the shared DAO omits the
// column and PostgreSQL's DEFAULT files the row into the deployment's default
// organization, which is the misfiling suite-identity#251 exists to prevent.
func (r *NotificationChannelRepository) Create(ctx context.Context, ch *NotificationChannel, opts ...identitynotify.ChannelWriteOption) (*NotificationChannel, error) {
	return r.inner.Create(ctx, ch, opts...)
}

// List returns every channel in the deployment, without the encrypted target.
//
// THE UNSCOPED TWIN. It exists so ListInScope's platform-admin branch has one
// statement to delegate to, and so unscoped_twin_class_test.go has something to
// name when a handler calls it: a twin that is absent is a twin the guard cannot
// report. No request handler may call this.
func (r *NotificationChannelRepository) List(ctx context.Context) ([]NotificationChannel, error) {
	return r.inner.List(ctx)
}

// GetByID returns one channel INCLUDING its encrypted target, or an error
// wrapping identity/store's ErrNotFound.
//
// The unscoped twin, on the same terms as List — and this is the one whose
// result carries the secret, so the scoped reader beside it is not a formality.
func (r *NotificationChannelRepository) GetByID(ctx context.Context, id string) (*NotificationChannel, error) {
	return r.inner.GetByID(ctx, id)
}

// Update replaces a channel's mutable fields; an empty encryptedTarget keeps the
// stored one. The unscoped twin.
func (r *NotificationChannelRepository) Update(ctx context.Context, id, name, typ string, events []string, enabled bool, encryptedTarget string) (*NotificationChannel, error) {
	return r.inner.Update(ctx, id, name, typ, events, enabled, encryptedTarget)
}

// Delete removes a channel. The unscoped twin.
func (r *NotificationChannelRepository) Delete(ctx context.Context, id string) error {
	return r.inner.Delete(ctx, id)
}
