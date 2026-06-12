// Package driftingest parses Terraform plan JSON (terraform show -json) pushed
// to the drift ingest endpoint into the same counts + summary shape the
// dispatched CI workflows compute with jq (see internal/api/drift_workflows.go),
// so ingested and dispatched drift render identically. Ported from ogtsm's
// driftingest with the summary adapted to drift_runs' [{address, actions}] form.
package driftingest

// Plan is the subset of a `terraform show -json` document the ingest path
// needs; all other top-level keys are ignored.
type Plan struct {
	ResourceChanges []ResourceChange `json:"resource_changes"`
}

// ResourceChange mirrors one entry of the plan's resource_changes array.
type ResourceChange struct {
	Address string `json:"address"`
	Change  Change `json:"change"`
}

// Change holds the planned actions for a single resource: ["no-op"],
// ["create"], ["update"], ["delete"], or ["delete","create"] for a replacement.
type Change struct {
	Actions []string `json:"actions"`
}

// SummaryEntry matches the rows of drift_runs.summary so the frontend renders
// both origins uniformly.
type SummaryEntry struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
}

// Result carries the counts and summary derived from a plan. Count semantics
// match the CI workflow's jq exactly: a resource counts as added/changed/
// destroyed when its actions CONTAIN create/update/delete respectively, so a
// replacement (delete+create) counts as both added and destroyed.
type Result struct {
	Added     int
	Changed   int
	Destroyed int
	Summary   []SummaryEntry
}

// Drifted reports whether the plan contained any non-no-op resource change.
func (r *Result) Drifted() bool {
	return len(r.Summary) > 0
}

// Summarize classifies each resource change. Entries whose actions are exactly
// ["no-op"] are excluded from the summary (the jq filter `!= ["no-op"]`);
// read-only refreshes (["read"]) appear in the summary but count toward
// nothing, again matching the workflow.
func Summarize(plan *Plan) *Result {
	res := &Result{Summary: []SummaryEntry{}}
	if plan == nil {
		return res
	}
	for _, rc := range plan.ResourceChanges {
		if hasAction(rc.Change.Actions, "create") {
			res.Added++
		}
		if hasAction(rc.Change.Actions, "update") {
			res.Changed++
		}
		if hasAction(rc.Change.Actions, "delete") {
			res.Destroyed++
		}
		if len(rc.Change.Actions) == 1 && rc.Change.Actions[0] == "no-op" {
			continue
		}
		res.Summary = append(res.Summary, SummaryEntry{Address: rc.Address, Actions: rc.Change.Actions})
	}
	return res
}

func hasAction(actions []string, action string) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}
