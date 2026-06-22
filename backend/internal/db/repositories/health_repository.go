package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// HealthRun tracks a version-lab run: a terraform init/plan against pinned
// versions, and whether it succeeded.
type HealthRun struct {
	ID                   string            `json:"id"`
	PipelineConnectionID *string           `json:"pipeline_connection_id"`
	RepoRef              string            `json:"repo_ref"`
	WorkingDir           string            `json:"working_dir"`
	TerraformVersion     string            `json:"terraform_version"`
	ProviderVersions     map[string]string `json:"provider_versions"`
	ModuleVersions       map[string]string `json:"module_versions"`
	RegistryHost         string            `json:"registry_host"`
	Status               string            `json:"status"`
	InitOK               *bool             `json:"init_ok"`
	PlanOK               *bool             `json:"plan_ok"`
	Success              *bool             `json:"success"`
	Summary              json.RawMessage   `json:"summary,omitempty"`
	Detail               string            `json:"detail"`
	CallbackToken        string            `json:"-"`
	Actor                string            `json:"actor"`
	CreatedAt            string            `json:"created_at"`
	UpdatedAt            string            `json:"updated_at"`
}

// HealthRepository is the DAO for health_runs.
type HealthRepository struct {
	db *sql.DB
}

func NewHealthRepository(db *sql.DB) *HealthRepository {
	return &HealthRepository{db: db}
}

const healthColumns = `id, pipeline_connection_id, COALESCE(repo_ref,''), COALESCE(working_dir,''),
	COALESCE(terraform_version,''), provider_versions, module_versions, COALESCE(registry_host,''), status,
	init_ok, plan_ok, success, summary, COALESCE(detail,''), callback_token, COALESCE(actor,''),
	created_at::text, updated_at::text`

func scanHealth(scanner interface{ Scan(dest ...any) error }) (*HealthRun, error) {
	var h HealthRun
	var connID sql.NullString
	var initOK, planOK, success sql.NullBool
	var providerVersions, moduleVersions, summary []byte
	if err := scanner.Scan(&h.ID, &connID, &h.RepoRef, &h.WorkingDir, &h.TerraformVersion, &providerVersions,
		&moduleVersions, &h.RegistryHost, &h.Status, &initOK, &planOK, &success, &summary, &h.Detail, &h.CallbackToken,
		&h.Actor, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	if connID.Valid {
		h.PipelineConnectionID = &connID.String
	}
	if len(providerVersions) > 0 {
		_ = json.Unmarshal(providerVersions, &h.ProviderVersions)
	}
	if len(moduleVersions) > 0 {
		_ = json.Unmarshal(moduleVersions, &h.ModuleVersions)
	}
	if initOK.Valid {
		v := initOK.Bool
		h.InitOK = &v
	}
	if planOK.Valid {
		v := planOK.Bool
		h.PlanOK = &v
	}
	if success.Valid {
		v := success.Bool
		h.Success = &v
	}
	if len(summary) > 0 {
		h.Summary = summary
	}
	return &h, nil
}

func (r *HealthRepository) Create(ctx context.Context, h *HealthRun) (*HealthRun, error) {
	pv, err := json.Marshal(orEmptyStrMap(h.ProviderVersions))
	if err != nil {
		return nil, err
	}
	mv, err := json.Marshal(orEmptyStrMap(h.ModuleVersions))
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO health_runs
			(pipeline_connection_id, repo_ref, working_dir, terraform_version, provider_versions, module_versions, registry_host, status, callback_token, actor)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9, $10)
		RETURNING `+healthColumns,
		h.PipelineConnectionID, nullStr(h.RepoRef), nullStr(h.WorkingDir), nullStr(h.TerraformVersion),
		string(pv), string(mv), nullStr(h.RegistryHost), h.Status, h.CallbackToken, nullStr(h.Actor))
	return scanHealth(row)
}

func (r *HealthRepository) GetByID(ctx context.Context, id string) (*HealthRun, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+healthColumns+` FROM health_runs WHERE id = $1`, id)
	h, err := scanHealth(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (r *HealthRepository) List(ctx context.Context, limit int) ([]HealthRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+healthColumns+` FROM health_runs ORDER BY created_at DESC LIMIT $1`, limit)
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

func (r *HealthRepository) UpdateStatus(ctx context.Context, id, status, detail string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE health_runs SET status=$2, detail=COALESCE(NULLIF($3,''), detail), updated_at=now() WHERE id=$1`,
		id, status, detail)
	return err
}

// ConsumeCallbackToken atomically clears the run's callback token if it still
// equals the supplied value, returning true when it did — making the machine
// callback one-shot (a replay finds the token already cleared and is rejected).
func (r *HealthRepository) ConsumeCallbackToken(ctx context.Context, id, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE health_runs SET callback_token='' WHERE id=$1 AND callback_token=$2`, id, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *HealthRepository) UpdateResult(ctx context.Context, id, status string, initOK, planOK, success bool, summary []byte, detail string) error {
	var summaryArg any
	if len(summary) > 0 {
		summaryArg = string(summary)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE health_runs
		SET status=$2, init_ok=$3, plan_ok=$4, success=$5, summary=$6::jsonb,
		    detail=COALESCE(NULLIF($7,''), detail), updated_at=now()
		WHERE id=$1`,
		id, status, initOK, planOK, success, summaryArg, detail)
	return err
}

// ListExpiredDispatched returns dispatched health runs created before cutoff —
// runs whose CI job never posted a result callback (build failed at init/plan,
// the agent crashed, the pipeline was cancelled). created_at is the dispatch time
// and never moves while a run is stuck in "dispatched", so it is the TTL anchor.
// The callback_token is retained (not blanked like List does) because the
// reconciler needs it to expire each run race-safely. Bounded by limit so one
// sweep cannot load an unbounded backlog; a larger backlog drains over subsequent
// sweeps.
func (r *HealthRepository) ListExpiredDispatched(ctx context.Context, cutoff time.Time, limit int) ([]HealthRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+healthColumns+`
		FROM health_runs WHERE status='dispatched' AND created_at < $1
		ORDER BY created_at ASC LIMIT $2`, cutoff, limit)
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
		out = append(out, *h)
	}
	return out, rows.Err()
}

// ExpireDispatched atomically fails a dispatched health run that never received a
// callback: in one statement it flips status dispatched->failed, records detail,
// and clears the callback token — but only if the run is STILL dispatched and
// still holds the supplied token. This mirrors ConsumeCallbackToken's
// compare-and-clear so expiry is race-safe against a real callback landing
// concurrently: that callback consumes the token first, so this matches zero rows
// and reports false (the caller then skips notifying). Clearing the token also
// makes any later/replayed callback fail the run's uniform 401 token check.
// Returns true only when this call performed the expiry.
func (r *HealthRepository) ExpireDispatched(ctx context.Context, id, token, detail string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE health_runs SET status='failed', detail=$3, callback_token='', updated_at=now()
		WHERE id=$1 AND status='dispatched' AND callback_token=$2`,
		id, token, detail)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func orEmptyStrMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
