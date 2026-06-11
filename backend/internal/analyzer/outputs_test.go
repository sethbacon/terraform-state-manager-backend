package analyzer

import (
	"strings"
	"testing"
)

func TestListOutputs(t *testing.T) {
	raw := `{
		"version": 4, "lineage": "lin-1", "serial": 7,
		"outputs": {
			"vpc_id": {"value": "vpc-123", "type": "string"},
			"subnet_ids": {"value": ["a","b"], "type": ["list", "string"]},
			"db_password": {"value": "hunter2", "type": "string", "sensitive": true},
			"config": {"value": {"region": "eu-west-1"}, "type": ["object", {"region": "string"}]}
		},
		"resources": []
	}`
	outputs, err := ListOutputs([]byte(raw))
	if err != nil {
		t.Fatalf("ListOutputs: %v", err)
	}
	if len(outputs) != 4 {
		t.Fatalf("outputs = %d, want 4", len(outputs))
	}
	// Sorted by name: config, db_password, subnet_ids, vpc_id.
	if outputs[0].Name != "config" || outputs[0].Type != "object" {
		t.Errorf("config: %+v", outputs[0])
	}
	if outputs[3].Name != "vpc_id" || outputs[3].Type != "string" || string(outputs[3].Value) != `"vpc-123"` {
		t.Errorf("vpc_id: %+v", outputs[3])
	}
	if outputs[2].Type != "list" {
		t.Errorf("subnet_ids type = %q, want list", outputs[2].Type)
	}

	// Sensitive: flagged, value REDACTED.
	sens := outputs[1]
	if sens.Name != "db_password" || !sens.Sensitive {
		t.Fatalf("db_password: %+v", sens)
	}
	if len(sens.Value) != 0 || strings.Contains(string(sens.Value), "hunter2") {
		t.Errorf("sensitive value leaked: %q", string(sens.Value))
	}
}

func TestListOutputs_EmptyAndInvalid(t *testing.T) {
	outputs, err := ListOutputs([]byte(`{"version":4,"lineage":"lin","resources":[]}`))
	if err != nil || len(outputs) != 0 {
		t.Errorf("no-outputs state: %v %v", outputs, err)
	}
	if _, err := ListOutputs([]byte(`not json`)); err == nil {
		t.Error("invalid JSON: expected error")
	}
	if _, err := ListOutputs([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("non-state JSON: expected error")
	}
}

func TestCtyTypeLabel(t *testing.T) {
	cases := map[string]string{
		`"string"`:                  "string",
		`["list","string"]`:         "list",
		`["object",{"a":"string"}]`: "object",
		`["map","number"]`:          "map",
		`12`:                        "complex",
		``:                          "",
	}
	for in, want := range cases {
		if got := ctyTypeLabel([]byte(in)); got != want {
			t.Errorf("ctyTypeLabel(%s) = %q, want %q", in, got, want)
		}
	}
}
