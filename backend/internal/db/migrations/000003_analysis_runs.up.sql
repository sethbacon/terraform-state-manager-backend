CREATE TABLE analysis_runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   UUID REFERENCES organizations(id) ON DELETE CASCADE,
    source_id         UUID REFERENCES state_sources(id) ON DELETE SET NULL,
    status            VARCHAR(50) NOT NULL DEFAULT 'pending',
    trigger_type      VARCHAR(50) NOT NULL,
    config            JSONB DEFAULT '{}',
    started_at        TIMESTAMP,
    completed_at      TIMESTAMP,
    total_workspaces  INT DEFAULT 0,
    successful_count  INT DEFAULT 0,
    failed_count      INT DEFAULT 0,
    total_rum         INT DEFAULT 0,
    total_managed     INT DEFAULT 0,
    total_resources   INT DEFAULT 0,
    total_data_sources INT DEFAULT 0,
    error_message     TEXT,
    performance_ms    INT,
    triggered_by      UUID REFERENCES users(id),
    created_at        TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_analysis_runs_org ON analysis_runs(organization_id);
CREATE INDEX idx_analysis_runs_status ON analysis_runs(status);
CREATE INDEX idx_analysis_runs_created ON analysis_runs(created_at DESC);

CREATE TABLE analysis_results (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES analysis_runs(id) ON DELETE CASCADE,
    workspace_id        VARCHAR(255),
    workspace_name      VARCHAR(255) NOT NULL,
    organization        VARCHAR(255),
    status              VARCHAR(50) NOT NULL,
    error_type          VARCHAR(50),
    error_message       TEXT,
    total_resources     INT DEFAULT 0,
    managed_count       INT DEFAULT 0,
    rum_count           INT DEFAULT 0,
    data_source_count   INT DEFAULT 0,
    null_resource_count INT DEFAULT 0,
    resources_by_type   JSONB DEFAULT '{}',
    resources_by_module JSONB DEFAULT '{}',
    provider_analysis   JSONB DEFAULT '{}',
    terraform_version   VARCHAR(50),
    state_serial        INT,
    state_lineage       VARCHAR(255),
    last_modified       TIMESTAMP,
    analysis_method     VARCHAR(50),
    raw_state_hash      VARCHAR(64),
    created_at          TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_analysis_results_run ON analysis_results(run_id);
CREATE INDEX idx_analysis_results_workspace ON analysis_results(workspace_name);
