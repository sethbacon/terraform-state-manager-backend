// Package driftingest parses Terraform plan JSON (terraform show -json) into the
// drift-change summary stored on a code-sourced drift event. It is the inbound
// counterpart to the snapshot-vs-snapshot drift detector: instead of diffing two
// state snapshots, it reads the planned actions a CI pipeline computed.
package driftingest

// Plan is the subset of a `terraform show -json` document the ingest path needs.
// Only resource_changes is consumed; other top-level keys are ignored.
type Plan struct {
	ResourceChanges []ResourceChange `json:"resource_changes"`
}

// ResourceChange mirrors one entry of the plan's resource_changes array.
type ResourceChange struct {
	Address string `json:"address"`
	Change  Change `json:"change"`
}

// Change holds the planned actions for a single resource. Actions is a list such
// as ["no-op"], ["create"], ["update"], ["delete"], or ["delete","create"] for a
// replacement.
type Change struct {
	Actions []string `json:"actions"`
}

// PlanChanges summarizes the resources a plan would change, keyed by resource
// address. It is serialized as the JSONB `changes` payload on the drift event.
// The field layout mirrors snapshot drift (added/removed/modified) so existing
// drift consumers can read code-sourced events uniformly.
type PlanChanges struct {
	Added         []string `json:"added"`
	Removed       []string `json:"removed"`
	Modified      []string `json:"modified"`
	ResourceDelta int      `json:"resource_delta"`
}

// HasChanges reports whether any non-no-op resource change was found.
func (pc *PlanChanges) HasChanges() bool {
	return len(pc.Added) > 0 || len(pc.Removed) > 0 || len(pc.Modified) > 0
}

// SummarizePlan classifies each resource_change by its planned actions:
//   - create            → Added
//   - delete            → Removed
//   - update            → Modified
//   - delete+create     → Modified (in-place replacement; net resource count unchanged)
//   - no-op / read      → ignored
//
// ResourceDelta is the net change in managed resource count (creates minus
// deletes; replacements are net-zero).
func SummarizePlan(plan *Plan) *PlanChanges {
	changes := &PlanChanges{
		Added:    make([]string, 0),
		Removed:  make([]string, 0),
		Modified: make([]string, 0),
	}
	if plan == nil {
		return changes
	}

	for _, rc := range plan.ResourceChanges {
		create := hasAction(rc.Change.Actions, "create")
		del := hasAction(rc.Change.Actions, "delete")
		update := hasAction(rc.Change.Actions, "update")

		switch {
		case create && del:
			// Replacement: resource is recreated in place, net count unchanged.
			changes.Modified = append(changes.Modified, rc.Address)
		case create:
			changes.Added = append(changes.Added, rc.Address)
			changes.ResourceDelta++
		case del:
			changes.Removed = append(changes.Removed, rc.Address)
			changes.ResourceDelta--
		case update:
			changes.Modified = append(changes.Modified, rc.Address)
		}
	}

	return changes
}

// hasAction reports whether actions contains the given action verb.
func hasAction(actions []string, action string) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}
