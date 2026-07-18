// Package notify is a thin adapter over the shared
// github.com/sethbacon/terraform-suite-identity/identity/notify package. All
// real logic (SMTP transport, channel fan-out, SSRF-safe delivery, secret
// redaction) lives in the shared package now — this file exists only to
// preserve this repo's existing call-site ergonomics (notify.New(repo, smtp,
// ...), notify.Event{Type: notify.EventXxx}) across drift.go/drift_records.go/
// health.go, per the cross-app notification parity effort.
package notify

import (
	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
	identitymailer "github.com/sethbacon/terraform-suite-identity/identity/mailer"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Notifier fans an Event out to admin-configured notification channels.
// Aliased directly to the shared type: SendTest/SendTestEmail/Notify are all
// defined there.
type Notifier = identitynotify.Notifier

// Event is a single alert-worthy occurrence to fan out to subscribed channels.
type Event = identitynotify.Event

// Event types worth alerting on.
const (
	EventDriftDetected = "drift_detected"
	EventRunFailed     = "run_failed"
)

// SMTPConfig is the shared outbound mail relay backing email channels. Aliased
// directly to the shared mailer.Config so no field-by-field conversion is
// needed at call sites that build one (router.go, tests).
type SMTPConfig = identitymailer.Config

// ParseRecipients is aliased to the shared implementation.
var ParseRecipients = identitynotify.ParseRecipients

// New builds a Notifier over the channel repository. smtp is held by
// reference — a runtime SMTP settings update (e.g. via PUT
// /notifications/smtp-config, which mutates *smtp in place) is observed by
// the Notifier on its next send without recreating it. tokenCipher decrypts
// channel targets at send time; guard applies the deployment's egress policy
// to every webhook/Slack/Teams POST (pass nil for the strict default policy
// — this app has no security.egress.allowlist equivalent config).
func New(repo *repositories.NotificationChannelRepository, smtp *SMTPConfig, tokenCipher *identitycrypto.TokenCipher, guard *identityhttpsafe.Guard) *Notifier {
	if smtp == nil {
		smtp = &SMTPConfig{}
	}
	provider := func() identitymailer.Config { return *smtp }
	opts := identitynotify.Options{
		Source:      "terraform-state-manager",
		TestMessage: "This is a test from Terraform State Manager.",
	}
	return identitynotify.NewNotifier(repo, provider, tokenCipher, guard, opts)
}
