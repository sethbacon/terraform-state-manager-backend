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
//
// The two sweeps share this struct because they answer the same two questions
// per row -- did it need work, and did the work land. They do NOT share a reason
// for skipping a row, and that difference is the whole of #364: bind-targets
// skips a row that is already bound, a rekey run skips one that is already on
// the current key, and a row can be the first without being the second.
// AlreadyBound therefore carries the count and `skipped` carries what it means,
// so a rekey run never prints "already-bound" over a row that still needs the
// key the operator is about to delete.
type Result struct {
	Total        int
	Converted    int
	AlreadyBound int
	Failed       int

	// skipped labels AlreadyBound in String(). Empty means the binding sweep's
	// wording, so a zero Result still prints sensibly.
	skipped string
}

func (r Result) String() string {
	skipped := r.skipped
	if skipped == "" {
		skipped = "already-bound"
	}
	return fmt.Sprintf("examined=%d converted=%d %s=%d failed=%d",
		r.Total, r.Converted, skipped, r.AlreadyBound, r.Failed)
}

// ErrUnboundRemain is returned by a verify run that found rows still unbound. It
// exists so the check is scriptable: verify is the gate that says whether the
// legacy read can be retired, and a gate that only prints is not a gate.
var ErrUnboundRemain = errors.New("maintenance: notification channel targets remain unbound")

// ErrNoTokenCipher is returned when no encryption key is configured.
//
// A sentinel rather than a bare error so a caller -- and the tests that have to
// prove the refusal happens rather than the sweep quietly finding nothing to do
// -- can tell "misconfigured" from "ran and reported".
var ErrNoTokenCipher = errors.New("maintenance: no token cipher configured (set TSM_ENCRYPTION_KEY)")

// sealedTarget is one candidate row: a row id and the ciphertext stored for it,
// in whichever form it currently has.
type sealedTarget struct{ id, sealed string }

// column is one AAD-bound secret column this service can sweep.
//
// There is exactly one today. It is a registry rather than three hard-coded
// closures because the rekey gate's scope is read from it: rekey_coverage_test.go
// checks, in both directions, that every AAD context function in the service is
// either covered by an entry here or explicitly declared uncovered. A second
// bound column added without an entry would silently narrow a gate that keeps
// reporting success, and success here is what an operator deletes a key on.
type column struct {
	// name appears in the logs; use table.column so a partial run is readable.
	name string
	// context derives the AAD binding for a row. It MUST call the same exported
	// function the application seals with, never rebuild the string, or the
	// sweep writes values the app cannot read.
	context func(id string) []byte
	// list returns every row holding a secret.
	list func(ctx context.Context, db *sql.DB) ([]sealedTarget, error)
	// update writes a converted ciphertext back for one row.
	update func(ctx context.Context, db *sql.DB, id, sealed string) error
}

var columns = []column{
	{
		name:    "notification_channels.encrypted_target",
		context: func(id string) []byte { return identitynotify.TargetContext(id) },
		list:    loadSealedTargets,
		update: func(ctx context.Context, db *sql.DB, id, sealed string) error {
			_, err := db.ExecContext(ctx,
				`UPDATE notification_channels SET encrypted_target = $2, updated_at = now() WHERE id = $1`,
				id, sealed)
			return err
		},
	},
}

// converter decides what a sweep does with one row. needsWork false means the
// row is already in the target state and must be left alone; otherwise
// replacement is the ciphertext to store.
//
// An error is a row-level failure -- reported, stepped over, and counted against
// the verify gate. It is never a reason to abandon the remaining rows.
type converter func(col column, row sealedTarget) (replacement string, needsWork bool, err error)

// sweep runs one converter over every registered column, writing unless
// verifyOnly, and reports whether any row needed work or failed.
//
// It is shared by BindChannelTargets and RekeyChannelTargets so that the
// properties an operator relies on -- verify writes nothing, one bad row does
// not abandon the rest, every registered column is walked -- are implemented
// once and cannot hold for one command and not the other. `skipped` is only the
// word the two use for a row they left alone.
//
// Writes are per row, not per sweep. A transaction spanning every credential
// would hold locks for the length of the run and turn a partial failure into an
// all-or-nothing one; a single-row UPDATE is already atomic, so an interrupted
// sweep leaves each row either wholly converted or wholly untouched, and both
// forms still open. That is what makes it resumable.
func sweep(
	ctx context.Context,
	db *sql.DB,
	verifyOnly bool,
	skipped string,
	convert converter,
) (Result, bool, error) {
	res := Result{skipped: skipped}

	for _, col := range columns {
		// Read the whole candidate set and close the cursor BEFORE any write.
		// The loop below issues UPDATEs, and holding a result set open across
		// them keeps a connection pinned for the length of the sweep.
		rows, err := col.list(ctx, db)
		if err != nil {
			return res, false, err
		}
		res.Total += len(rows)

		for _, row := range rows {
			replacement, needsWork, err := convert(col, row)
			if err != nil {
				// One bad row must not abandon the rest: a target that cannot
				// be decrypted at all (wrong key, corruption, or a value bound
				// to another row) is a pre-existing problem this sweep did not
				// cause and cannot fix.
				res.Failed++
				slog.Error("channel target could not be converted",
					"column", col.name, "channel_id", row.id, "error", err)
				continue
			}
			if !needsWork {
				res.AlreadyBound++
				continue
			}
			if verifyOnly {
				res.Converted++ // would convert
				continue
			}
			if err := col.update(ctx, db, row.id, replacement); err != nil {
				res.Failed++
				slog.Error("converted channel target could not be written",
					"column", col.name, "channel_id", row.id, "error", err)
				continue
			}
			res.Converted++
		}
	}

	return res, res.Converted > 0 || res.Failed > 0, nil
}

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
// That skip is binding-only, and deliberately so: OpenWithContext falls back to
// the previous key, so a row bound before a key rotation opens here and is left
// where it is. Completing a rotation is RekeyChannelTargets' job, not this one's
// (#364).
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
	if tc == nil {
		return Result{}, ErrNoTokenCipher
	}

	res, unbound, err := sweep(ctx, db, verifyOnly, "already-bound",
		func(col column, row sealedTarget) (string, bool, error) {
			// Already bound? The cheapest and most reliable check available: try
			// to open it under its own context. No schema flag can answer this,
			// because the form is a property of the ciphertext, not of the row.
			if _, err := tc.OpenWithContext(row.sealed, col.context(row.id)); err == nil {
				return "", false, nil
			}
			// ReSealWithContext opens through the legacy path (including the
			// previous-key rotation fallback) and re-seals bound, without the
			// plaintext leaving the call.
			bound, err := tc.ReSealWithContext(row.sealed, col.context(row.id))
			return bound, true, err
		})
	if err != nil {
		return res, err
	}

	if verifyOnly && unbound {
		return res, fmt.Errorf("%w: %s", ErrUnboundRemain, res)
	}
	return res, nil
}

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
