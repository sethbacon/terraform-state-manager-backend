// Package maintenance holds operator-invoked, one-shot data tasks that cannot be
// expressed as SQL migrations.
package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
)

// Result is what one sweep did. Total is the rows examined, not the rows changed.
type Result struct {
	Total        int
	Converted    int
	AlreadyBound int
	Failed       int
}

func (r Result) String() string {
	return fmt.Sprintf("examined=%d converted=%d already-bound=%d failed=%d",
		r.Total, r.Converted, r.AlreadyBound, r.Failed)
}

// ErrUnboundRemain is returned by a verify run that found rows still unbound. It
// exists so the check is scriptable: verify is the gate that says whether the
// legacy read can be retired, and a gate that only prints is not a gate.
var ErrUnboundRemain = errors.New("maintenance: notification channel targets remain unbound")

// BindChannelTargets converts notification_channels.encrypted_target from the
// unbound form to one bound to its own row (suite-identity #153).
//
// This cannot be a SQL migration: AES-GCM re-encryption needs the key, which
// only exists in the running application. It is an operator-invoked one-shot
// rather than a startup hook or a worker, because it would otherwise need a
// cross-replica claim to avoid every replica sweeping at once — and this table
// is small enough that the scheduling machinery would cost more risk than it
// saves.
//
// Safe to re-run and safe to interrupt. A row already bound is detected and
// skipped rather than double-sealed, so a partial sweep resumes by simply being
// run again.
//
// When verifyOnly is set nothing is written; the sweep reports what WOULD be
// converted and returns ErrUnboundRemain if anything is still unbound. That is
// the exit criterion for this migration: while any row is unbound the notifier
// must keep accepting unbound ciphertexts through OpenWithContextOrLegacy, which
// is the property being retired. Once verify reports zero, the read can move to
// OpenWithContext and the legacy acceptance can be deleted.
func BindChannelTargets(
	ctx context.Context,
	db *sql.DB,
	tc *identitycrypto.TokenCipher,
	verifyOnly bool,
) (Result, error) {
	var res Result
	if tc == nil {
		return res, errors.New("maintenance: no token cipher configured (set TSM_ENCRYPTION_KEY)")
	}

	// Read the whole candidate set and close the cursor BEFORE any write. The
	// conversion loop below issues UPDATEs, and holding a result set open across
	// them keeps a connection pinned for the length of the sweep. Loading first
	// is also what lets the close be a single deferred call rather than one per
	// early return.
	all, err := loadSealedTargets(ctx, db)
	if err != nil {
		return res, err
	}
	res.Total = len(all)

	var todo []sealedTarget
	for _, row := range all {
		// Already bound? The cheapest and most reliable check available: try to
		// open it under its own context. No schema flag can answer this, because
		// the form is a property of the ciphertext, not of the row.
		if _, err := tc.OpenWithContext(row.sealed, identitynotify.TargetContext(row.id)); err == nil {
			res.AlreadyBound++
			continue
		}
		todo = append(todo, row)
	}

	for _, p := range todo {
		// ReSealWithContext opens through the legacy path (including the
		// previous-key rotation fallback) and re-seals bound, without the
		// plaintext leaving the call.
		bound, err := tc.ReSealWithContext(p.sealed, identitynotify.TargetContext(p.id))
		if err != nil {
			// One bad row must not abandon the rest: a target that cannot be
			// decrypted at all (wrong key, corruption) is a pre-existing problem
			// this sweep did not cause and cannot fix.
			res.Failed++
			slog.Error("channel target could not be converted", "channel_id", p.id, "error", err)
			continue
		}
		if verifyOnly {
			res.Converted++ // would convert
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE notification_channels SET encrypted_target = $2, updated_at = now() WHERE id = $1`,
			p.id, bound,
		); err != nil {
			res.Failed++
			slog.Error("channel target conversion could not be written", "channel_id", p.id, "error", err)
			continue
		}
		res.Converted++
	}

	if verifyOnly && (res.Converted > 0 || res.Failed > 0) {
		return res, fmt.Errorf("%w: %s", ErrUnboundRemain, res)
	}
	return res, nil
}

// sealedTarget is one candidate row: a channel id and the ciphertext stored for
// it, in whichever form it currently has.
type sealedTarget struct{ id, sealed string }

// loadSealedTargets reads every channel that actually holds a secret.
//
// Split out so the cursor is closed by a single deferred call rather than at
// each early return -- the previous shape had three explicit Close calls whose
// errors were discarded implicitly, which gosec flags as G104 and which is a
// real (if minor) smell: a Close error on a read is worth ignoring, but it
// should be ignored ON PURPOSE and visibly.
//
// An empty encrypted_target means "unset", not "unbound", so it is excluded at
// the query rather than filtered later; it is not a migration candidate.
func loadSealedTargets(ctx context.Context, db *sql.DB) ([]sealedTarget, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, encrypted_target FROM notification_channels WHERE encrypted_target <> ''`)
	if err != nil {
		return nil, fmt.Errorf("maintenance: list channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []sealedTarget
	for rows.Next() {
		var row sealedTarget
		if err := rows.Scan(&row.id, &row.sealed); err != nil {
			return nil, fmt.Errorf("maintenance: scan channel: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("maintenance: iterate channels: %w", err)
	}
	return out, nil
}
