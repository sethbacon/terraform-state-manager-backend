package models

import "time"

// Source-repo-link discovery methods. Manual is the operator-set link; Auto is
// reserved for live ADO auto-discovery (deferred, credential-gated).
const (
	RepoLinkDiscoveryManual = "manual"
	RepoLinkDiscoveryAuto   = "auto"
)

// SourceRepoLink binds a state source to the Azure DevOps repo (and optional
// plan pipeline) that owns its Terraform configuration. There is at most one
// link per source. It is consumed by the outbound drift-trigger (which pipeline
// to queue for a source) and by repo-metadata analysis (which repo to read).
type SourceRepoLink struct {
	ID                 string    `db:"id" json:"id"`
	OrganizationID     string    `db:"organization_id" json:"organization_id"`
	SourceID           string    `db:"source_id" json:"source_id"`
	ADOOrganizationURL string    `db:"ado_organization_url" json:"ado_organization_url"`
	ADOProject         string    `db:"ado_project" json:"ado_project"`
	ADORepo            string    `db:"ado_repo" json:"ado_repo"`
	ADOPipelineID      *int      `db:"ado_pipeline_id" json:"ado_pipeline_id,omitempty"`
	DiscoveryMethod    string    `db:"discovery_method" json:"discovery_method"`
	CreatedAt          time.Time `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time `db:"updated_at" json:"updated_at"`
}

// SourceRepoLinkRequest is the API binding for setting (creating or replacing) a
// source's repo link. discovery_method is server-controlled ("manual" for this
// endpoint) and is not accepted from the client.
type SourceRepoLinkRequest struct {
	ADOOrganizationURL string `json:"ado_organization_url" binding:"required"`
	ADOProject         string `json:"ado_project" binding:"required"`
	ADORepo            string `json:"ado_repo" binding:"required"`
	ADOPipelineID      *int   `json:"ado_pipeline_id,omitempty"`
}
