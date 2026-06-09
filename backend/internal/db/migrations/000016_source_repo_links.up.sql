-- Source-to-Azure-DevOps repo/pipeline linkage. One link per state source binds
-- a source to the ADO repo (and optional plan pipeline) that owns its Terraform
-- configuration. This link is consumed by the outbound drift-trigger (which
-- pipeline to queue for a source) and by repo-metadata analysis (which repo to
-- read). The discovery_method records whether the link was set manually or
-- resolved by live ADO auto-discovery (the latter is deferred; "manual" today).

CREATE TABLE source_repo_links (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id      UUID REFERENCES organizations(id) ON DELETE CASCADE,
    source_id            UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    ado_organization_url VARCHAR(500) NOT NULL,
    ado_project          VARCHAR(255) NOT NULL,
    ado_repo             VARCHAR(255) NOT NULL,
    ado_pipeline_id      INT,
    discovery_method     VARCHAR(20) NOT NULL DEFAULT 'manual',
    created_at           TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMP NOT NULL DEFAULT NOW(),
    -- One link per source: set/get/delete operate on the source's single link.
    UNIQUE (source_id)
);
CREATE INDEX idx_source_repo_links_org ON source_repo_links(organization_id);
