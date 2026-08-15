// notification_repository.go aliases the NotificationChannel model and
// ChannelRepository DAO from the shared identity/notify package: admin-
// configured delivery destinations (webhook, Slack, or an ad-hoc email
// recipient list) for drift_detected / run_failed alerts. Requires the
// notification_channels table to be on the shared schema (encrypted_target
// TEXT, events JSONB) — see migration 000023.
package repositories

import identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"

// NotificationChannel is a destination for alerts. The target is held
// encrypted (EncryptedTarget) and never serialized; HasTarget reports whether
// one is set so the UI can show "configured" without exposing the secret.
// Note: ID is a plain string and EncryptedTarget is a plain string (base64,
// via the shared TokenCipher) — this repo's prior BYTEA/[]byte target has
// been migrated to TEXT (see migration 000023).
type NotificationChannel = identitynotify.NotificationChannel

// NotificationChannelRepository is the DAO for notification_channels.
type NotificationChannelRepository = identitynotify.ChannelRepository

// NewNotificationChannelRepository constructs the repository over the app connection.
var NewNotificationChannelRepository = identitynotify.NewChannelRepository
