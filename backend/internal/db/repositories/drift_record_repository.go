package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DriftRecord is a durable, acknowledgeable drift condition on one state file.
// Re-detections collapse onto the live (non-resolved) record for the state; a
// clean result resolves it. drift_runs stay the per-run mechanism underneath.
type DriftRecord struct {
	ID                   string          `json:"id"`
	SourceID             *string         `json:"source_id"`
	StateKey             string          `json:"state_key"`
	PipelineConnectionID *string         `json:"pipeline_connection_id"`
	LastRunID            *string         `json:"last_run_id"`
	Origin               string          `json:"origin"`   // run | ingest
	Severity             string          `json:"severity"` // critical | warning
	Added                int             `json:"added"`
	Changed              int             `json:"changed"`
	Destroyed            int             `json:"destroyed"`
	Summary              json.RawMessage `json:"summary,omitempty"`
	Status               string          `json:"status"` // open | acknowledged | resolved
	AcknowledgedBy       string          `json:"acknowledged_by"`
	AcknowledgedAt       *string         `json:"acknowledged_at"`
	AckNote              string          `json:"ack_note"`
	ResolvedAt           *string         `json:"resolved_at"`
	ExternalRef          *string         `json:"external_ref,omitempty"`
	Detections           int             `json:"detections"`
	FirstDetectedAt      string          `json:"first_detected_at"`
	LastDetectedAt       string          `json:"last_detected_at"`

	// Completeness markers from the drift contract, describing what the check
	// did NOT do. Promoted, so the record's JSON keys are unchanged.
	Completeness
}

// DriftSeverity classifies drift the way ogtsm did: destroyed resources are
// critical, anything else that drifted is a warning.
func DriftSeverity(destroyed int) string {
	if destroyed > 0 {
		return "critical"
	}
	return "warning"
}

// DriftRecordRepository is the DAO for drift_records.
type DriftRecordRepository struct {
	db *sql.DB
}

func NewDriftRecordRepository(db *sql.DB) *DriftRecordRepository {
	return &DriftRecordRepository{db: db}
}

const driftRecordColumns = `id, source_id, state_key, pipeline_connection_id, last_run_id, origin, severity,
	added, changed, destroyed, summary, status, acknowledged_by, acknowledged_at::text, ack_note,
	resolved_at::text, external_ref, detections, first_detected_at::text, last_detected_at::text,
	truncated, omitted_entries, omitted_attrs, unparseable, unmasked`

func scanDriftRecord(scanner interface{ Scan(dest ...any) error }) (*DriftRecord, error) {
	var r DriftRecord
	var srcID, connID, runID, ackAt, resolvedAt, extRef sql.NullString
	var summary []byte
	if err := scanner.Scan(&r.ID, &srcID, &r.StateKey, &connID, &runID, &r.Origin, &r.Severity,
		&r.Added, &r.Changed, &r.Destroyed, &summary, &r.Status, &r.AcknowledgedBy, &ackAt, &r.AckNote,
		&resolvedAt, &extRef, &r.Detections, &r.FirstDetectedAt, &r.LastDetectedAt,
		&r.Truncated, &r.OmittedEntries, &r.OmittedAttrs, &r.Unparseable, &r.Unmasked); err != nil {
		return nil, err
	}
	if srcID.Valid {
		r.SourceID = &srcID.String
	}
	if connID.Valid {
		r.PipelineConnectionID = &connID.String
	}
	if runID.Valid {
		r.LastRunID = &runID.String
	}
	if ackAt.Valid {
		r.AcknowledgedAt = &ackAt.String
	}
	if resolvedAt.Valid {
		r.ResolvedAt = &resolvedAt.String
	}
	if extRef.Valid {
		r.ExternalRef = &extRef.String
	}
	if len(summary) > 0 {
		r.Summary = summary
	}
	return &r, nil
}

// Detection carries one drift observation into UpsertDetection.
type Detection struct {
	SourceID             string
	StateKey             string
	PipelineConnectionID *string
	RunID                *string
	Origin               string // run | ingest
	Added                int
	Changed              int
	Destroyed            int
	Summary              []byte
	ExternalRef          *string

	// Completeness markers, carried from whichever producer observed the plan.
	// Overwritten (not accumulated) on re-detection, exactly like the counts
	// beside them: the record describes the LATEST observation, so a later
	// complete check must be able to clear an earlier truncated one.
	Completeness
}

// UpsertDetection records a drift observation: it updates the live
// (non-resolved) record for the state — counts, summary, last_detected_at,
// detections — or inserts a fresh open one. Acknowledged records stay
// acknowledged on re-detection. A replayed ingest (same source + external_ref,
// already resolved) is returned as-is instead of erroring.
// UpsertDetection records a drift detection, inheriting the organization from
// the SOURCE the detection is about.
//
// # Why the organization is derived here rather than passed in
//
// This statement has TWO producers — the drift callback (recordDriftOutcome) and
// the external /drift/ingest endpoint — and its ON CONFLICT collapses them onto
// ONE row. So whichever inserts first fixes that record's organization
// permanently; organization_id is deliberately absent from the DO UPDATE SET
// below, because a later detection must never re-parent an existing record.
//
// If the two producers computed the organization independently and ever
// disagreed, a live record's tenant would be decided by a race. Deriving it from
// the source INSIDE the statement removes that possibility rather than
// documenting it: both producers reach the same parent by construction, and
// there is no window between reading the source and writing the record.
//
// # Why the SOURCE, not the run
//
// A drift record's identity is (source_id, state_key), and BOTH its unique
// indexes are keyed on source_id — 000033 says they follow the source and need
// no re-keying in Phase 4. A record whose organization disagreed with its
// source's would be a finding invisible to the organization that owns the state
// file it is about. The run is the right CROSS-CHECK and the caller has it in
// hand, but it is not the parent: /drift/ingest can arrive without one.
//
// # An unstamped or missing source yields NO ROW
//
// The INSERT selects from state_sources with `organization_id IS NOT NULL`, so a
// source that does not exist, or one that predates its stamp, produces zero rows
// and this returns ErrSourceNotOwned. That is deliberate: the alternative is a
// drift record with a NULL organization, which is invisible to every tenant —
// a finding nobody can see is worse than a refused write.
func (r *DriftRecordRepository) UpsertDetection(ctx context.Context, d *Detection) (*DriftRecord, error) {
	var summaryArg any
	if len(d.Summary) > 0 {
		summaryArg = string(d.Summary)
	}
	d.MarkTruncation()
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO drift_records
			(source_id, state_key, pipeline_connection_id, last_run_id, origin, severity,
			 added, changed, destroyed, summary, external_ref,
			 truncated, omitted_entries, omitted_attrs, unparseable, unmasked, organization_id)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10::jsonb,'[]'::jsonb), $11,
			 $12, $13, $14, $15, $16, s.organization_id
		FROM state_sources s
		WHERE s.id = $1 AND s.organization_id IS NOT NULL
		ON CONFLICT (source_id, state_key) WHERE status <> 'resolved'
		DO UPDATE SET
			pipeline_connection_id = COALESCE(EXCLUDED.pipeline_connection_id, drift_records.pipeline_connection_id),
			last_run_id      = COALESCE(EXCLUDED.last_run_id, drift_records.last_run_id),
			origin           = EXCLUDED.origin,
			severity         = EXCLUDED.severity,
			added            = EXCLUDED.added,
			changed          = EXCLUDED.changed,
			destroyed        = EXCLUDED.destroyed,
			summary          = EXCLUDED.summary,
			external_ref     = COALESCE(EXCLUDED.external_ref, drift_records.external_ref),
			truncated        = EXCLUDED.truncated,
			omitted_entries  = EXCLUDED.omitted_entries,
			omitted_attrs    = EXCLUDED.omitted_attrs,
			unparseable      = EXCLUDED.unparseable,
			unmasked         = EXCLUDED.unmasked,
			detections       = drift_records.detections + 1,
			last_detected_at = now()
		RETURNING `+driftRecordColumns,
		d.SourceID, d.StateKey, d.PipelineConnectionID, d.RunID, d.Origin, DriftSeverity(d.Destroyed),
		d.Added, d.Changed, d.Destroyed, summaryArg, d.ExternalRef,
		d.Truncated, d.OmittedEntries, d.OmittedAttrs, d.Unparseable, d.Unmasked)
	rec, err := scanDriftRecord(row)
	if err != nil {
		// A resolved record can still hold this external_ref (pipeline retry
		// after auto-resolve): treat the replay as idempotent, not an error.
		var pqErr *pgconn.PgError
		if errors.As(err, &pqErr) && pqErr.Code == "23505" && d.ExternalRef != nil {
			if existing, gErr := r.GetByExternalRef(ctx, d.SourceID, *d.ExternalRef); gErr == nil && existing != nil {
				return existing, nil
			}
		}
		// ZERO ROWS means the SELECT found no source with an organization: the
		// source id does not exist, or it predates its stamp. Named, because the
		// alternative reading — "the upsert failed" — sends an operator looking
		// at drift when the fault is an unowned source (#436).
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: source %s", ErrSourceNotOwned, d.SourceID)
		}
		return nil, fmt.Errorf("failed to upsert drift record: %w", err)
	}
	return rec, nil
}

// GetByExternalRef returns the record carrying an ingest idempotency key, or
// (nil, nil) when none exists.
func (r *DriftRecordRepository) GetByExternalRef(ctx context.Context, sourceID, externalRef string) (*DriftRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+driftRecordColumns+` FROM drift_records WHERE source_id = $1 AND external_ref = $2`,
		sourceID, externalRef)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ResolveClean closes the live record for a state after a clean result,
// returning whether one was open.
func (r *DriftRecordRepository) ResolveClean(ctx context.Context, sourceID, stateKey string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE drift_records SET status='resolved', resolved_at=now()
		WHERE source_id = $1 AND state_key = $2 AND status <> 'resolved'`,
		sourceID, stateKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// driftRecordFilter renders the shared WHERE tail for List/CountRecords. The
// clause only ever gains fixed SQL with positional placeholders; all
// caller-supplied values bind through the returned args.
func driftRecordFilter(statuses []string, sourceID, severity string, start, end *time.Time) (string, []any) {
	clause := ""
	args := []any{}
	if len(statuses) > 0 {
		args = append(args, statuses)
		clause += fmt.Sprintf(" AND status = ANY($%d)", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if sourceID != "" {
		args = append(args, sourceID)
		clause += fmt.Sprintf(" AND source_id = $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if severity != "" {
		args = append(args, severity)
		clause += fmt.Sprintf(" AND severity = $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if start != nil {
		args = append(args, *start)
		clause += fmt.Sprintf(" AND last_detected_at >= $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if end != nil {
		args = append(args, *end)
		clause += fmt.Sprintf(" AND last_detected_at <= $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	return clause, args
}

// List returns records newest-detection-first, windowed by limit/offset. Empty
// filter values mean "any"; statuses filters to the given set; start/end bound
// last_detected_at. Use CountRecords with the same filters for the total.
func (r *DriftRecordRepository) List(ctx context.Context, statuses []string, sourceID, severity string, limit, offset int, start, end *time.Time) ([]DriftRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	clause, args := driftRecordFilter(statuses, sourceID, severity, start, end)
	q := `SELECT ` + driftRecordColumns + ` FROM drift_records WHERE 1=1` + clause // #nosec G202 -- fixed SQL + placeholder-only clause from driftRecordFilter; no interpolated values
	args = append(args, limit, offset)
	q += fmt.Sprintf(" ORDER BY last_detected_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)) // #nosec G202 -- placeholder only

	rows, err := r.db.QueryContext(ctx, q, args...) // #nosec G202 -- q is built from fixed SQL + placeholders above
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DriftRecord{}
	for rows.Next() {
		rec, err := scanDriftRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// CountRecords returns the record total for the same filters as List.
func (r *DriftRecordRepository) CountRecords(ctx context.Context, statuses []string, sourceID, severity string, start, end *time.Time) (int, error) {
	clause, args := driftRecordFilter(statuses, sourceID, severity, start, end)
	var n int
	// #nosec G202 -- clause is fixed SQL with positional placeholders; values bound via args
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_records WHERE 1=1`+clause, args...).Scan(&n)
	return n, err
}

// GetByID returns one record, or (nil, nil) when absent.
func (r *DriftRecordRepository) GetByID(ctx context.Context, id string) (*DriftRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+driftRecordColumns+` FROM drift_records WHERE id = $1`, id)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Acknowledge transitions an OPEN record to acknowledged, recording who/when
// and an optional note. Returns (nil, nil) when the record is missing or not
// open (callers distinguish via GetByID).
func (r *DriftRecordRepository) Acknowledge(ctx context.Context, id, actor, note string) (*DriftRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE drift_records
		SET status='acknowledged', acknowledged_by=$2, acknowledged_at=now(), ack_note=$3
		WHERE id = $1 AND status = 'open'
		RETURNING `+driftRecordColumns,
		id, actor, note)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Resolve manually closes a record (e.g. drift remediated out-of-band).
// Returns (nil, nil) when the record is missing or already resolved.
func (r *DriftRecordRepository) Resolve(ctx context.Context, id string) (*DriftRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE drift_records SET status='resolved', resolved_at=now()
		WHERE id = $1 AND status <> 'resolved'
		RETURNING `+driftRecordColumns, id)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// CountsByStatus returns record counts keyed by status (dashboard badge).
func (r *DriftRecordRepository) CountsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM drift_records GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
