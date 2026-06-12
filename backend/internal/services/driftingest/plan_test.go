package driftingest

import (
	"encoding/json"
	"testing"
)

func TestSummarize_CountsMatchWorkflowJQ(t *testing.T) {
	// Semantics contract: counts are action MEMBERSHIP (a replacement counts as
	// added AND destroyed), summary excludes exactly-["no-op"] entries — the
	// same results the dispatched CI workflow computes with jq.
	planJSON := `{
		"format_version": "1.2",
		"resource_changes": [
			{"address": "aws_instance.new",      "change": {"actions": ["create"]}},
			{"address": "aws_instance.tweak",    "change": {"actions": ["update"]}},
			{"address": "aws_instance.gone",     "change": {"actions": ["delete"]}},
			{"address": "aws_instance.replaced", "change": {"actions": ["delete", "create"]}},
			{"address": "aws_instance.same",     "change": {"actions": ["no-op"]}},
			{"address": "data.aws_ami.x",        "change": {"actions": ["read"]}}
		]
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	res := Summarize(&plan)
	if res.Added != 2 || res.Changed != 1 || res.Destroyed != 2 {
		t.Errorf("counts = +%d ~%d -%d, want +2 ~1 -2", res.Added, res.Changed, res.Destroyed)
	}
	if len(res.Summary) != 5 { // everything except the no-op
		t.Errorf("summary entries = %d, want 5 (%+v)", len(res.Summary), res.Summary)
	}
	if !res.Drifted() {
		t.Error("plan with changes must report drifted")
	}
	// Replacement keeps its full action list for the UI.
	for _, e := range res.Summary {
		if e.Address == "aws_instance.replaced" && len(e.Actions) != 2 {
			t.Errorf("replacement actions = %v", e.Actions)
		}
	}
}

func TestSummarize_NoOpAndNilPlans(t *testing.T) {
	res := Summarize(&Plan{ResourceChanges: []ResourceChange{
		{Address: "aws_instance.same", Change: Change{Actions: []string{"no-op"}}},
	}})
	if res.Drifted() || res.Added+res.Changed+res.Destroyed != 0 {
		t.Errorf("no-op plan must be clean: %+v", res)
	}

	if res := Summarize(nil); res.Drifted() || res.Summary == nil {
		t.Errorf("nil plan must yield an empty, clean result: %+v", res)
	}

	// A read-only refresh appears in the summary (matching jq) and therefore
	// reports drifted=true only via summary presence — counts stay zero.
	res = Summarize(&Plan{ResourceChanges: []ResourceChange{
		{Address: "data.aws_ami.x", Change: Change{Actions: []string{"read"}}},
	}})
	if res.Added+res.Changed+res.Destroyed != 0 || len(res.Summary) != 1 {
		t.Errorf("read entry handling: %+v", res)
	}
}
