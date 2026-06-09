package ado

import (
	"context"
	"fmt"
)

// Run represents an Azure DevOps pipeline run as returned by the runs API. Only
// the fields the trigger flow needs are modelled; the wire response carries more.
type Run struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url"`
}

// QueuePipelineRunRequest describes a run to queue for a given pipeline.
//
// Branch is optional; when set, the run is queued against that source branch
// (e.g. "refs/heads/main"). Parameters is an optional map of pipeline runtime
// parameters passed through to templateParameters. PipelineID is the numeric id
// of the pipeline to run (the source↔pipeline link lookup is a separate slice;
// callers supply the id directly for now).
type QueuePipelineRunRequest struct {
	PipelineID int
	Branch     string
	Parameters map[string]string
}

// queueRunBody is the wire body for POST _apis/pipelines/{id}/runs. Empty fields
// are omitted so a parameter-less run against the pipeline's default branch sends
// a minimal body.
type queueRunBody struct {
	Resources          *runResources     `json:"resources,omitempty"`
	TemplateParameters map[string]string `json:"templateParameters,omitempty"`
}

type runResources struct {
	Repositories map[string]runRepository `json:"repositories"`
}

type runRepository struct {
	RefName string `json:"refName"`
}

// QueuePipelineRun triggers a run of the given pipeline by calling
// POST {org}/{project}/_apis/pipelines/{pipelineId}/runs (api-version 7.1) and
// returns the queued run. This is how the outbound drift trigger kicks off a
// plan pipeline in Azure DevOps; the resulting plan JSON is delivered back to
// TSM via the inbound drift-ingest endpoint.
//
// A non-2xx response is returned as an *APIError (inspectable via IsConflict),
// mirroring the other write methods.
func (c *Client) QueuePipelineRun(ctx context.Context, req QueuePipelineRunRequest) (*Run, error) {
	if req.PipelineID <= 0 {
		return nil, fmt.Errorf("ado: PipelineID is required to queue a run")
	}

	body := queueRunBody{TemplateParameters: req.Parameters}
	if req.Branch != "" {
		body.Resources = &runResources{
			Repositories: map[string]runRepository{
				"self": {RefName: req.Branch},
			},
		}
	}

	var run Run
	path := c.projectPath(fmt.Sprintf("_apis/pipelines/%d/runs", req.PipelineID))
	if err := c.postJSON(ctx, path, defaultParams(), body, &run); err != nil {
		return nil, fmt.Errorf("queuing run for pipeline %d: %w", req.PipelineID, err)
	}
	return &run, nil
}
