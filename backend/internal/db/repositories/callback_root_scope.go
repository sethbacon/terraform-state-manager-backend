// callback_root_scope.go is the Phase 3 read flip for the three CALLBACK ROOTS
// — drift_runs, health_runs and drift_records (#393, tracking #502).
//
// # Why these three are grouped, and why they came last
//
// The six roots flipped before them are read by exactly one kind of caller: a
// person, through a request that resolves a tenant scope. These three are read
// by two, and the second has no principal at all. A CI job posts its plan result
// to /drift/runs/:id/results holding a per-run bearer credential and nothing
// else — no session, no membership, no organization header. So "resolve the
// caller's scope" is not a question that can be asked on that path, and the
// answer #393 (option B, 2026-08-29) gives is that the credential IS the
// authority: authenticate the token, and the run it authenticates carries the
// organization every statement afterwards runs under.
//
// Both paths land here. The readers below take a tenantscope.Scope and do not
// care which of the two produced it, which is the point: a scope derived from an
// authenticated run and a scope resolved from a request are the same type and go
// through the same predicate, so there is one place where "may this caller see
// this row" is decided rather than two that will disagree.
//
// # The predicate excludes NULL, on all three
//
// `NULL = ANY(...)` is NULL, never true. 000034 made the column NOT NULL, but a
// database restored from an older backup still holds unstamped rows, and this is
// the layer that has to keep working when the constraint above it is absent. An
// unstamped row is therefore invisible to every tenant rather than visible to
// all of them, and reachable only by a platform admin — which is what keeps it
// repairable instead of merely lost.
//
// # Why drift_runs and health_runs read their OWN column and never a join
//
// Migration 000033 argues this at both tables and it is worth restating where
// the predicate lives: both parents of a drift run (source_id,
// pipeline_connection_id) and the only parent of a health run
// (pipeline_connection_id) are nullable ON DELETE SET NULL. Delete the source
// and the run survives, still holding its state_key, its plan summary and its
// callback_token. An inherited answer would be NULL for exactly those rows —
// unpartitioned, which is to say readable by everyone — so the durable answer
// has to be the run's own column.
//
// drift_records is the one that gets asked about most, because 000033 describes
// it as following the source. It does, at INSERT time and only there:
// UpsertDetection reads s.organization_id out of state_sources inside the
// statement, so both of its producers reach the same parent by construction and
// a race cannot decide a record's tenant. Once written, the record has its own
// column and the READ predicate is on that column alone — which is what keeps a
// record readable after its source_id has gone NULL, the case 000033 says the
// column exists for.
package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// runOrgPredicate is the tenant filter for drift_runs and health_runs. Both bind
// the organization array as $1, so the two readers share one predicate string
// and a parameter reordering in either shows up as a bound-argument mismatch
// rather than as a by-id read filtered on the wrong value.
const runOrgPredicate = `organization_id = ANY($1::uuid[])`

// runPage clamps a runs page window. Shared by the scoped and unscoped readers
// so a page bound cannot differ between the two — a scoped reader with a
// different cap would return a different row count for the same request and read
// as a scoping bug.
func runPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ---------------------------------------------------------------------------
// drift_runs
// ---------------------------------------------------------------------------

// ListInScope returns the drift runs the scope permits, newest first.
//
// An empty scope reads NOTHING, without a query. The early return is not an
// optimisation: it states the fail-closed answer where a later edit cannot
// change it by accident, because a query with an empty id list happens to match
// nothing today and that is a fact about PostgreSQL rather than a decision.
func (r *DriftRepository) ListInScope(ctx context.Context, limit, offset int, f DriftRunFilter, scope tenantscope.Scope) ([]DriftRun, error) {
	if scope.Empty() {
		return []DriftRun{}, nil
	}
	if scope.PlatformAdmin {
		return r.List(ctx, limit, offset, f)
	}
	limit, offset = runPage(limit, offset)
	clause, args := driftRunFilterClause([]any{scope.OrgIDs}, f)
	q := `SELECT ` + driftColumns + ` FROM drift_runs WHERE ` + runOrgPredicate + clause // #nosec G202 -- fixed SQL + placeholder-only clause from driftRunFilterClause; no interpolated values
	args = append(args, limit, offset)
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)) // #nosec G202 -- placeholder only

	rows, err := r.db.QueryContext(ctx, q, args...) // #nosec G202 -- q is built from fixed SQL + placeholders above
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DriftRun{}
	for rows.Next() {
		d, err := scanDrift(rows)
		if err != nil {
			return nil, err
		}
		d.CallbackToken = "" // never expose in lists, exactly as List does
		out = append(out, *d)
	}
	return out, rows.Err()
}

// CountRunsInScope is the total for the same window ListInScope pages over.
//
// Scoped alongside the list rather than left alone: an unscoped total next to a
// scoped page is a disclosure in its own right — "showing 3 of 47" tells a
// tenant how many runs the other organizations in the deployment have.
func (r *DriftRepository) CountRunsInScope(ctx context.Context, f DriftRunFilter, scope tenantscope.Scope) (int, error) {
	if scope.Empty() {
		return 0, nil
	}
	if scope.PlatformAdmin {
		return r.CountRuns(ctx, f)
	}
	clause, args := driftRunFilterClause([]any{scope.OrgIDs}, f)
	var n int
	// #nosec G202 -- clause is fixed SQL with positional placeholders; values bound via args
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_runs WHERE `+runOrgPredicate+clause, args...).Scan(&n)
	return n, err
}

// GetByIDInScope returns one drift run when the scope permits it, and
// (nil, nil) otherwise.
//
// A run in another organization is reported EXACTLY as a run that does not
// exist. The run row carries the plan summary — the resource addresses being
// changed and destroyed — so "that one is not yours" would both disclose the
// existence of the run and let a caller enumerate ids.
func (r *DriftRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*DriftRun, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+driftColumns+`
		FROM drift_runs WHERE `+runOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	d, err := scanDrift(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// LatestPerStateInScope is DriftRepository.LatestPerState, scoped: the newest
// run per state_key for one source, restricted to organizations the scope
// reaches. Backs the Phase 4a coverage endpoint, which has already verified
// the source itself is in scope (SourceRepository.GetByIDInScope) before
// calling this -- the organization predicate here is defense in depth against
// a run whose own organization_id ever disagreed with its source's.
func (r *DriftRepository) LatestPerStateInScope(ctx context.Context, sourceID string, scope tenantscope.Scope) (map[string]DriftRun, error) {
	if scope.Empty() {
		return map[string]DriftRun{}, nil
	}
	if scope.PlatformAdmin {
		return r.LatestPerState(ctx, sourceID)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (state_key) `+driftColumns+`
		FROM drift_runs WHERE `+runOrgPredicate+` AND source_id = $2
		ORDER BY state_key, created_at DESC`, scope.OrgIDs, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DriftRun{}
	for rows.Next() {
		d, err := scanDrift(rows)
		if err != nil {
			return nil, err
		}
		d.CallbackToken = ""
		out[d.StateKey] = *d
	}
	return out, rows.Err()
}

// UpdateResultInScope records a drift outcome only when the scope reaches the
// run's organization.
//
// THE WRITE SIDE OF THE CALLBACK, and it is not ceremony. The run id this binds
// is taken from the authenticated run today, so under the current handler the
// predicate is tautological — and that is precisely the property a later edit
// takes away, by reading the id from the URL or the body instead. A tenancy
// partition that only holds while nobody moves one line is not a partition; the
// statement says who may write it.
func (r *DriftRepository) UpdateResultInScope(ctx context.Context, id, status string, added, changed, destroyed int, drifted bool, summary []byte, detail string, marks Completeness, infra InfraDrift, scope tenantscope.Scope) error {
	w := scopeWrite(scope)
	if w.Deny {
		return ErrNotInScope
	}
	if w.Skip {
		return r.UpdateResult(ctx, id, status, added, changed, destroyed, drifted, summary, detail, marks, infra)
	}
	var summaryArg any
	if len(summary) > 0 {
		summaryArg = string(summary)
	}
	var infraSummaryArg any
	if len(infra.Summary) > 0 {
		infraSummaryArg = string(infra.Summary)
	}
	marks.MarkTruncation()
	_, err := r.db.ExecContext(ctx, `
		UPDATE drift_runs
		SET status=$2, added=$3, changed=$4, destroyed=$5, drifted=$6, summary=$7::jsonb,
		    detail=COALESCE(NULLIF($8,''), detail), updated_at=now(),
		    truncated=$9, omitted_entries=$10, omitted_attrs=$11, unparseable=$12, unmasked=$13,
		    drift_added=$14, drift_changed=$15, drift_destroyed=$16, drift_summary=$17::jsonb
		WHERE id=$1 AND organization_id = ANY($18::uuid[])`,
		id, status, added, changed, destroyed, drifted, summaryArg, detail,
		marks.Truncated, marks.OmittedEntries, marks.OmittedAttrs, marks.Unparseable, marks.Unmasked,
		infra.Added, infra.Changed, infra.Destroyed, infraSummaryArg,
		w.OrgIDs)
	return err
}

// ---------------------------------------------------------------------------
// health_runs
// ---------------------------------------------------------------------------

// ListInScope returns the health runs the scope permits, newest first. Same
// shape and same reasons as DriftRepository.ListInScope above.
func (r *HealthRepository) ListInScope(ctx context.Context, limit, offset int, status string, scope tenantscope.Scope) ([]HealthRun, error) {
	if scope.Empty() {
		return []HealthRun{}, nil
	}
	if scope.PlatformAdmin {
		return r.List(ctx, limit, offset, status)
	}
	limit, offset = runPage(limit, offset)
	var (
		rows *sql.Rows
		err  error
	)
	if status != "" {
		rows, err = r.db.QueryContext(ctx, `SELECT `+healthColumns+`
			FROM health_runs WHERE `+runOrgPredicate+` AND status = $2
			ORDER BY created_at DESC LIMIT $3 OFFSET $4`, scope.OrgIDs, status, limit, offset)
	} else {
		rows, err = r.db.QueryContext(ctx, `SELECT `+healthColumns+`
			FROM health_runs WHERE `+runOrgPredicate+`
			ORDER BY created_at DESC LIMIT $2 OFFSET $3`, scope.OrgIDs, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthRun{}
	for rows.Next() {
		h, err := scanHealth(rows)
		if err != nil {
			return nil, err
		}
		h.CallbackToken = ""
		out = append(out, *h)
	}
	return out, rows.Err()
}

// CountRunsInScope is the total for the window ListInScope pages over.
func (r *HealthRepository) CountRunsInScope(ctx context.Context, status string, scope tenantscope.Scope) (int, error) {
	if scope.Empty() {
		return 0, nil
	}
	if scope.PlatformAdmin {
		return r.CountRuns(ctx, status)
	}
	var n int
	var err error
	if status != "" {
		err = r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM health_runs WHERE `+runOrgPredicate+` AND status = $2`, scope.OrgIDs, status).Scan(&n)
	} else {
		err = r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM health_runs WHERE `+runOrgPredicate, scope.OrgIDs).Scan(&n)
	}
	return n, err
}

// GetByIDInScope returns one health run when the scope permits it, and
// (nil, nil) otherwise.
func (r *HealthRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*HealthRun, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+healthColumns+`
		FROM health_runs WHERE `+runOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	h, err := scanHealth(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

// UpdateResultInScope records a health outcome only when the scope reaches the
// run's organization. See DriftRepository.UpdateResultInScope for why the
// currently-tautological predicate is written down rather than assumed.
func (r *HealthRepository) UpdateResultInScope(ctx context.Context, id, status string, initOK, planOK, success bool, summary []byte, detail string, scope tenantscope.Scope) error {
	w := scopeWrite(scope)
	if w.Deny {
		return ErrNotInScope
	}
	if w.Skip {
		return r.UpdateResult(ctx, id, status, initOK, planOK, success, summary, detail)
	}
	var summaryArg any
	if len(summary) > 0 {
		summaryArg = string(summary)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE health_runs
		SET status=$2, init_ok=$3, plan_ok=$4, success=$5, summary=$6::jsonb,
		    detail=COALESCE(NULLIF($7,''), detail), updated_at=now()
		WHERE id=$1 AND organization_id = ANY($8::uuid[])`,
		id, status, initOK, planOK, success, summaryArg, detail, w.OrgIDs)
	return err
}

// ---------------------------------------------------------------------------
// drift_records
// ---------------------------------------------------------------------------

// driftRecordOrgPredicate is the tenant filter for drift_records. It binds $1,
// which is why driftRecordFilter takes the args slice already seeded.
const driftRecordOrgPredicate = `organization_id = ANY($1::uuid[])`

// ListInScope returns the drift records the scope permits, newest-detection
// first, under the same filters List takes.
func (r *DriftRecordRepository) ListInScope(ctx context.Context, statuses []string, sourceID, severity string, limit, offset int, start, end *time.Time, scope tenantscope.Scope) ([]DriftRecord, error) {
	if scope.Empty() {
		return []DriftRecord{}, nil
	}
	if scope.PlatformAdmin {
		return r.List(ctx, statuses, sourceID, severity, limit, offset, start, end)
	}
	limit, offset = recordPage(limit, offset)
	clause, args := driftRecordFilter([]any{scope.OrgIDs}, statuses, sourceID, severity, start, end)
	q := `SELECT ` + driftRecordColumns + ` FROM drift_records WHERE ` + driftRecordOrgPredicate + clause // #nosec G202 -- fixed SQL + placeholder-only clause from driftRecordFilter; no interpolated values
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

// CountRecordsInScope is the filtered total for ListInScope's paging.
func (r *DriftRecordRepository) CountRecordsInScope(ctx context.Context, statuses []string, sourceID, severity string, start, end *time.Time, scope tenantscope.Scope) (int, error) {
	if scope.Empty() {
		return 0, nil
	}
	if scope.PlatformAdmin {
		return r.CountRecords(ctx, statuses, sourceID, severity, start, end)
	}
	clause, args := driftRecordFilter([]any{scope.OrgIDs}, statuses, sourceID, severity, start, end)
	var n int
	// #nosec G202 -- clause is fixed SQL with positional placeholders; values bound via args
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_records WHERE `+driftRecordOrgPredicate+clause, args...).Scan(&n)
	return n, err
}

// CountsByStatusInScope is the status tally behind the drift page's chips.
//
// Scoped for the reason the list total is: an unscoped tally beside a scoped
// list says "12 open" to a tenant who can see three of them, which both
// discloses another organization's finding count and reads as a bug.
func (r *DriftRecordRepository) CountsByStatusInScope(ctx context.Context, scope tenantscope.Scope) (map[string]int, error) {
	if scope.Empty() {
		return map[string]int{}, nil
	}
	if scope.PlatformAdmin {
		return r.CountsByStatus(ctx)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM drift_records WHERE `+driftRecordOrgPredicate+` GROUP BY status`, scope.OrgIDs)
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

// LiveByStateInScope is DriftRecordRepository.LiveByState, scoped: every
// non-resolved record for one source, restricted to organizations the scope
// reaches. Backs the Phase 4a coverage endpoint alongside
// DriftRepository.LatestPerStateInScope, for the same defense-in-depth reason
// given there.
func (r *DriftRecordRepository) LiveByStateInScope(ctx context.Context, sourceID string, scope tenantscope.Scope) (map[string]DriftRecord, error) {
	if scope.Empty() {
		return map[string]DriftRecord{}, nil
	}
	if scope.PlatformAdmin {
		return r.LiveByState(ctx, sourceID)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+driftRecordColumns+`
		FROM drift_records WHERE `+driftRecordOrgPredicate+` AND source_id = $2 AND status <> 'resolved'`,
		scope.OrgIDs, sourceID)
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

// CountsBySourceInScope is the drift summary's per-source breakdown (GET
// /drift/summary), restricted to organizations the scope reaches. The
// organization predicate is bound on BOTH sides of the join — the record's own
// column and the source's — belt and suspenders around the same join
// UpsertDetectionInScope makes on the write side, for the case those two ever
// disagreed.
func (r *DriftRecordRepository) CountsBySourceInScope(ctx context.Context, scope tenantscope.Scope) ([]SourceRecordCounts, error) {
	if scope.Empty() {
		return []SourceRecordCounts{}, nil
	}
	if scope.PlatformAdmin {
		return r.CountsBySource(ctx)
	}
	rows, err := r.db.QueryContext(ctx, countsBySourceQuery+`
		AND r.organization_id = ANY($1::uuid[]) AND s.organization_id = ANY($1::uuid[])
		GROUP BY r.source_id, s.name ORDER BY s.name`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourceRecordCounts(rows)
}

// CountIncompleteInScope is the drift summary's incomplete_records field,
// restricted to organizations the scope reaches. An unscoped count here would
// tell a tenant how many checks failed to finish across the WHOLE deployment,
// which is the same class of disclosure CountRecordsInScope's total guards
// against.
func (r *DriftRecordRepository) CountIncompleteInScope(ctx context.Context, scope tenantscope.Scope) (int, error) {
	if scope.Empty() {
		return 0, nil
	}
	if scope.PlatformAdmin {
		return r.CountIncomplete(ctx)
	}
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM drift_records WHERE `+driftRecordOrgPredicate+`
		 AND status <> 'resolved' AND (unparseable OR truncated)`, scope.OrgIDs).Scan(&n)
	return n, err
}

// CountInfraDriftedInScope is the drift summary's infra_drifted field,
// restricted to organizations the scope reaches -- CountIncompleteInScope's
// twin, for the same disclosure reason: an unscoped count would tell a tenant
// how many findings carry infra drift across the WHOLE deployment.
func (r *DriftRecordRepository) CountInfraDriftedInScope(ctx context.Context, scope tenantscope.Scope) (int, error) {
	if scope.Empty() {
		return 0, nil
	}
	if scope.PlatformAdmin {
		return r.CountInfraDrifted(ctx)
	}
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM drift_records WHERE `+driftRecordOrgPredicate+`
		 AND status <> 'resolved' AND (drift_added > 0 OR drift_changed > 0 OR drift_destroyed > 0)`,
		scope.OrgIDs).Scan(&n)
	return n, err
}

// GetByIDInScope returns one drift record when the scope permits it.
//
// A record in another organization is reported EXACTLY as one that does not
// exist. This is the highest-value row in the set — the acknowledgeable
// statement of what is currently wrong with somebody's infrastructure, resource
// addresses included — so the not-found shape matters more here than anywhere.
func (r *DriftRecordRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*DriftRecord, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+driftRecordColumns+`
		FROM drift_records WHERE `+driftRecordOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// AcknowledgeInScope transitions an OPEN record to acknowledged only when the
// scope reaches its organization.
//
// The third side of the partition (INSERT, read, and write) on the table where
// forgetting it is worst: an acknowledgement is the statement that a human has
// SEEN a finding, so a cross-organization acknowledge silences another tenant's
// live drift under a name from outside their organization.
func (r *DriftRecordRepository) AcknowledgeInScope(ctx context.Context, id, actor, note string, scope tenantscope.Scope) (*DriftRecord, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return nil, ErrNotInScope
	}
	if w.Skip {
		return r.Acknowledge(ctx, id, actor, note)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE drift_records
		SET status='acknowledged', acknowledged_by=$2, acknowledged_at=now(), ack_note=$3
		WHERE id = $1 AND status = 'open' AND organization_id = ANY($4::uuid[])
		RETURNING `+driftRecordColumns,
		id, actor, note, w.OrgIDs)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ResolveInScope manually closes a record only when the scope reaches its
// organization. Closing another tenant's live finding is the destructive half of
// the same defect AcknowledgeInScope closes.
func (r *DriftRecordRepository) ResolveInScope(ctx context.Context, id string, scope tenantscope.Scope) (*DriftRecord, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return nil, ErrNotInScope
	}
	if w.Skip {
		return r.Resolve(ctx, id)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE drift_records SET status='resolved', resolved_at=now()
		WHERE id = $1 AND status <> 'resolved' AND organization_id = ANY($2::uuid[])
		RETURNING `+driftRecordColumns, id, w.OrgIDs)
	rec, err := scanDriftRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ResolveCleanInScope closes the live record for a state after a clean result,
// only when the scope reaches the record's organization.
//
// THIS IS THE ONE THE MACHINE CALLBACK NEEDS. A drift run's source_id is
// nullable and nothing in the schema forces it to name a source in the run's own
// organization; the dispatch chain refuses a crossing target, but a row written
// before that landed, or by direct SQL, still can. Unscoped, a clean result
// posted with a run's own callback token would then resolve ANOTHER
// organization's open drift record — closing a live finding on infrastructure
// the caller has nothing to do with, silently, and with no record that anything
// was refused.
func (r *DriftRecordRepository) ResolveCleanInScope(ctx context.Context, sourceID, stateKey string, scope tenantscope.Scope) (bool, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return false, ErrNotInScope
	}
	if w.Skip {
		return r.ResolveClean(ctx, sourceID, stateKey)
	}
	return execAffectedOne(ctx, r.db, `
		UPDATE drift_records SET status='resolved', resolved_at=now()
		WHERE source_id = $1 AND state_key = $2 AND status <> 'resolved'
		  AND organization_id = ANY($3::uuid[])`,
		sourceID, stateKey, w.OrgIDs)
}

// UpsertDetectionInScope records a drift observation only when the SOURCE the
// detection is about is one the scope reaches.
//
// The organization still comes from the source, inside the statement, for the
// reason UpsertDetection gives at length: two producers collapse onto one row
// through the ON CONFLICT, so computing the value outside would let a race
// decide a live record's tenant. What this adds is the entitlement check on the
// same SELECT — the source must be in scope for the row to be produced at all —
// so the value's provenance is unchanged and the write is refused rather than
// re-parented when the two disagree.
//
// A source outside the scope yields zero rows and ErrSourceNotOwned, exactly as
// an unstamped one does. The two are deliberately the same answer: both mean
// "this detection has no owner you may write to", and distinguishing them would
// tell a caller which source ids are real elsewhere in the deployment.
func (r *DriftRecordRepository) UpsertDetectionInScope(ctx context.Context, d *Detection, scope tenantscope.Scope) (*DriftRecord, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return nil, ErrNotInScope
	}
	if w.Skip {
		return r.UpsertDetection(ctx, d)
	}
	return r.upsertDetection(ctx, d, ` AND s.organization_id = ANY($21::uuid[])`, w.OrgIDs)
}
