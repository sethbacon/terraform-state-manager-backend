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

	// DriftAdded/DriftChanged/DriftDestroyed/DriftSummary are the drift
	// contract's second triplet (resource_drift, migration 000039) -- see the
	// identical fields on DriftRun for what they mean and why they are plain
	// int rather than pointers.
	DriftAdded     int             `json:"drift_added"`
	DriftChanged   int             `json:"drift_changed"`
	DriftDestroyed int             `json:"drift_destroyed"`
	DriftSummary   json.RawMessage `json:"drift_summary,omitempty"`

	// Completeness markers from the drift contract, describing what the check
	// did NOT do. Promoted, so the record's JSON keys are unchanged.
	Completeness
	// OrganizationID is the owning tenant, carried in memory so an alert can be
	// fanned out to THAT organization's notification channels and no others.
	// Never serialized: the boundary is enforced server-side (#459).
	OrganizationID string `json:"-"`
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
	truncated, omitted_entries, omitted_attrs, unparseable, unmasked,
	organization_id::text,
	COALESCE(drift_added,0), COALESCE(drift_changed,0), COALESCE(drift_destroyed,0), drift_summary`

func scanDriftRecord(scanner interface{ Scan(dest ...any) error }) (*DriftRecord, error) {
	var r DriftRecord
	var organizationID sql.NullString
	var srcID, connID, runID, ackAt, resolvedAt, extRef sql.NullString
	var summary, driftSummary []byte
	if err := scanner.Scan(&r.ID, &srcID, &r.StateKey, &connID, &runID, &r.Origin, &r.Severity,
		&r.Added, &r.Changed, &r.Destroyed, &summary, &r.Status, &r.AcknowledgedBy, &ackAt, &r.AckNote,
		&resolvedAt, &extRef, &r.Detections, &r.FirstDetectedAt, &r.LastDetectedAt,
		&r.Truncated, &r.OmittedEntries, &r.OmittedAttrs, &r.Unparseable, &r.Unmasked,
		&organizationID,
		&r.DriftAdded, &r.DriftChanged, &r.DriftDestroyed, &driftSummary); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		r.OrganizationID = organizationID.String
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
	if len(driftSummary) > 0 {
		r.DriftSummary = driftSummary
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

	// Infra is the contract's second triplet -- drift outside Terraform
	// (resource_drift), as opposed to Added/Changed/Destroyed above (a plan's
	// resource_changes). Zero value on a producer that predates
	// terraform-drift-contract 1.3.0. See migration 000039.
	Infra InfraDrift
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
	return r.upsertDetection(ctx, d, "")
}

// upsertDetection is the one copy of the statement, with an optional EXTRA
// predicate on the source SELECT.
//
// Written once because the scoped and unscoped forms differ by a single AND, and
// a second hand-written copy of a statement whose ON CONFLICT decides a record's
// tenant permanently is exactly the drift this file's own comment warns about.
// sourceFilter is fixed SQL supplied by this package -- never a caller value --
// and everything variable binds through extra.
func (r *DriftRecordRepository) upsertDetection(ctx context.Context, d *Detection, sourceFilter string, extra ...any) (*DriftRecord, error) {
	var summaryArg any
	if len(d.Summary) > 0 {
		summaryArg = string(d.Summary)
	}
	var infraSummaryArg any
	if len(d.Infra.Summary) > 0 {
		infraSummaryArg = string(d.Infra.Summary)
	}
	d.MarkTruncation()
	args := []any{
		d.SourceID, d.StateKey, d.PipelineConnectionID, d.RunID, d.Origin, DriftSeverity(d.Destroyed),
		d.Added, d.Changed, d.Destroyed, summaryArg, d.ExternalRef,
		d.Truncated, d.OmittedEntries, d.OmittedAttrs, d.Unparseable, d.Unmasked,
		d.Infra.Added, d.Infra.Changed, d.Infra.Destroyed, infraSummaryArg,
	}
	args = append(args, extra...)
	// #nosec G202 -- sourceFilter is fixed SQL from this package (a bound
	// placeholder, never a caller value); every value binds through args.
	q := `
		INSERT INTO drift_records
			(source_id, state_key, pipeline_connection_id, last_run_id, origin, severity,
			 added, changed, destroyed, summary, external_ref,
			 truncated, omitted_entries, omitted_attrs, unparseable, unmasked,
			 drift_added, drift_changed, drift_destroyed, drift_summary, organization_id)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10::jsonb,'[]'::jsonb), $11,
			 $12, $13, $14, $15, $16,
			 $17, $18, $19, $20::jsonb, s.organization_id
		FROM state_sources s
		WHERE s.id = $1 AND s.organization_id IS NOT NULL` + sourceFilter + `
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
			drift_added      = EXCLUDED.drift_added,
			drift_changed    = EXCLUDED.drift_changed,
			drift_destroyed  = EXCLUDED.drift_destroyed,
			drift_summary    = EXCLUDED.drift_summary,
			detections       = drift_records.detections + 1,
			last_detected_at = now()
		RETURNING ` + driftRecordColumns
	row := r.db.QueryRowContext(ctx, q, args...) // #nosec G202 -- q assembled above from fixed SQL only
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

// recordPage clamps a records page window. Shared by the scoped and unscoped
// readers so a page bound cannot differ between the two.
func recordPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// driftRecordFilter renders the shared WHERE tail for List/CountRecords. The
// clause only ever gains fixed SQL with positional placeholders; all
// caller-supplied values bind through the returned args.
// args is SEEDED by the caller rather than started empty, so the scoped readers
// can bind the organization array as $1 and have every filter placeholder
// numbered after it. Renumbering by hand in a second copy of this function is
// how a by-id read ends up filtered on somebody else's value.
func driftRecordFilter(args []any, statuses []string, sourceID, severity string, start, end *time.Time) (string, []any) {
	clause := ""
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
	limit, offset = recordPage(limit, offset)
	clause, args := driftRecordFilter(nil, statuses, sourceID, severity, start, end)
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
	clause, args := driftRecordFilter(nil, statuses, sourceID, severity, start, end)
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

// LiveByState returns, for one source, every NON-resolved drift record keyed
// by state_key -- the Phase 4a coverage endpoint's other join, alongside
// DriftRepository.LatestPerState. Unscoped: callers join it against a
// source_id already verified in scope (LiveByStateInScope).
func (r *DriftRecordRepository) LiveByState(ctx context.Context, sourceID string) (map[string]DriftRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+driftRecordColumns+`
		FROM drift_records WHERE source_id = $1 AND status <> 'resolved'`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DriftRecord{}
	for rows.Next() {
		rec, err := scanDriftRecord(rows)
		if err != nil {
			return nil, err
		}
		out[rec.StateKey] = *rec
	}
	return out, rows.Err()
}

// PruneResolved bounds drift_records (Phase 4a, #567): it deletes resolved
// records older than maxAge. Unlike PruneRuns/PruneBackups this has no keep
// floor -- a resolved record is already a closed history entry rather than a
// live state's only restore point, so there is nothing an age cap alone could
// take that mattered to today's coverage. Returns the number removed.
func (r *DriftRecordRepository) PruneResolved(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, fmt.Errorf("drift record retention max age must be > 0, got %s", maxAge)
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM drift_records WHERE status='resolved' AND resolved_at < now() - make_interval(secs => $1)`,
		maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountOpenBySeverity returns the number of OPEN drift records by severity,
// deployment-wide. Backs tsm_drift_records_open{severity} (Phase 2's deferred
// metric, implemented here), refreshed once per reconciler tick -- unscoped
// for the same reason CountRunsIn is: it is an operational gauge, not a
// response to any single tenant's request.
func (r *DriftRecordRepository) CountOpenBySeverity(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT severity, COUNT(*) FROM drift_records WHERE status='open' GROUP BY severity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var severity string
		var n int
		if err := rows.Scan(&severity, &n); err != nil {
			return nil, err
		}
		out[severity] = n
	}
	return out, rows.Err()
}

// SourceRecordCounts is one source's row in the drift summary's per-source
// breakdown (GET /drift/summary's records_by_source).
type SourceRecordCounts struct {
	SourceID     string `json:"source_id"`
	SourceName   string `json:"source_name"`
	Open         int    `json:"open"`
	Acknowledged int    `json:"acknowledged"`
	Critical     int    `json:"critical"`
	// InfraDrift is how many of this source's LIVE records carry infra drift
	// (drift_added/drift_changed/drift_destroyed > 0, migration 000039) --
	// resource_drift, as opposed to Open/Acknowledged/Critical above, which
	// classify the record regardless of which triplet produced it. A record
	// counted here may also be counted in Open or Critical; the two are
	// independent facets of the same live finding, not alternatives.
	InfraDrift int `json:"infra_drift"`
}

// countsBySourceQuery is the ONE statement CountsBySource and CountsBySource
// InScope share, with an optional extra predicate appended (fixed SQL from
// this package only, exactly as driftRecordFilter's callers do) so the two
// cannot drift into disagreeing about what "live" or "critical" mean.
//
// Only LIVE (non-resolved) records are grouped: a resolved record contributes
// zero to every one of the four counts, so including it would only add
// meaningless all-zero rows for sources whose drift has all been closed.
const countsBySourceQuery = `
	SELECT r.source_id, COALESCE(s.name,''),
		COUNT(*) FILTER (WHERE r.status='open'),
		COUNT(*) FILTER (WHERE r.status='acknowledged'),
		COUNT(*) FILTER (WHERE r.severity='critical' AND r.status<>'resolved'),
		COUNT(*) FILTER (WHERE r.drift_added>0 OR r.drift_changed>0 OR r.drift_destroyed>0)
	FROM drift_records r
	JOIN state_sources s ON s.id = r.source_id
	WHERE r.status <> 'resolved'`

func scanSourceRecordCounts(rows *sql.Rows) ([]SourceRecordCounts, error) {
	out := []SourceRecordCounts{}
	for rows.Next() {
		var c SourceRecordCounts
		if err := rows.Scan(&c.SourceID, &c.SourceName, &c.Open, &c.Acknowledged, &c.Critical, &c.InfraDrift); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountsBySource is the drift summary's per-source breakdown, deployment-wide.
// Callers reach this only through CountsBySourceInScope's PlatformAdmin
// branch; every other caller resolves a scope first.
func (r *DriftRecordRepository) CountsBySource(ctx context.Context) ([]SourceRecordCounts, error) {
	rows, err := r.db.QueryContext(ctx, countsBySourceQuery+`
		GROUP BY r.source_id, s.name ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourceRecordCounts(rows)
}

// CountIncomplete returns the number of LIVE drift records whose own check did
// not finish (unparseable or truncated) -- the drift summary's
// incomplete_records field, deployment-wide. Callers reach this only through
// CountIncompleteInScope's PlatformAdmin branch.
func (r *DriftRecordRepository) CountIncomplete(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM drift_records WHERE status <> 'resolved' AND (unparseable OR truncated)`).Scan(&n)
	return n, err
}

// CountInfraDrifted returns the number of LIVE drift records carrying infra
// drift (drift_added/drift_changed/drift_destroyed > 0, migration 000039) --
// the drift summary's infra_drifted field, deployment-wide, mirroring
// CountIncomplete exactly. Callers reach this only through
// CountInfraDriftedInScope's PlatformAdmin branch.
func (r *DriftRecordRepository) CountInfraDrifted(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM drift_records
		WHERE status <> 'resolved' AND (drift_added > 0 OR drift_changed > 0 OR drift_destroyed > 0)`).Scan(&n)
	return n, err
}
