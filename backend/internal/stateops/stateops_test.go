package stateops

import (
	"encoding/json"
	"testing"
)

const state = `{
  "version": 4,
  "terraform_version": "1.7.5",
  "serial": 5,
  "lineage": "abc",
  "outputs": {"x": {"value": 1}},
  "resources": [
    {"mode":"managed","type":"aws_instance","name":"web","provider":"p","instances":[{"k":1}],"extra":"keepme"},
    {"module":"module.vpc","mode":"managed","type":"aws_subnet","name":"private","provider":"p","instances":[{}]},
    {"mode":"data","type":"aws_ami","name":"ubuntu","provider":"p","instances":[{}]}
  ]
}`

func parse(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestRemoveResource(t *testing.T) {
	out, err := RemoveResource([]byte(state), "aws_instance.web")
	if err != nil {
		t.Fatalf("RemoveResource: %v", err)
	}
	m := parse(t, out)
	res := m["resources"].([]any)
	if len(res) != 2 {
		t.Fatalf("expected 2 resources after rm, got %d", len(res))
	}
	if m["serial"].(float64) != 6 {
		t.Errorf("expected serial bumped to 6, got %v", m["serial"])
	}
	// Preserved fields on other resources.
	if m["outputs"] == nil {
		t.Error("outputs were dropped")
	}
}

func TestRemoveInModule(t *testing.T) {
	out, err := RemoveResource([]byte(state), "module.vpc.aws_subnet.private")
	if err != nil {
		t.Fatalf("RemoveResource (module): %v", err)
	}
	if len(parse(t, out)["resources"].([]any)) != 2 {
		t.Error("module resource not removed")
	}
}

func TestRemoveNotFound(t *testing.T) {
	if _, err := RemoveResource([]byte(state), "aws_instance.nope"); err == nil {
		t.Error("expected not-found error")
	}
}

func TestMoveResource(t *testing.T) {
	out, err := MoveResource([]byte(state), "aws_instance.web", "aws_instance.web2")
	if err != nil {
		t.Fatalf("MoveResource: %v", err)
	}
	m := parse(t, out)
	res := m["resources"].([]any)
	found := false
	for _, r := range res {
		rm := r.(map[string]any)
		if rm["type"] == "aws_instance" && rm["name"] == "web2" {
			found = true
			if rm["extra"] != "keepme" {
				t.Error("resource fields not preserved on move")
			}
		}
		if rm["name"] == "web" {
			t.Error("old name still present")
		}
	}
	if !found {
		t.Error("moved resource not found under new name")
	}
}

func TestMoveIntoModule(t *testing.T) {
	out, err := MoveResource([]byte(state), "aws_instance.web", "module.app.aws_instance.web")
	if err != nil {
		t.Fatalf("MoveResource into module: %v", err)
	}
	for _, r := range parse(t, out)["resources"].([]any) {
		rm := r.(map[string]any)
		if rm["type"] == "aws_instance" && rm["name"] == "web" {
			if rm["module"] != "module.app" {
				t.Errorf("expected module.app, got %v", rm["module"])
			}
		}
	}
}

func TestMoveTargetClash(t *testing.T) {
	if _, err := MoveResource([]byte(state), "aws_instance.web", "module.vpc.aws_subnet.private"); err == nil {
		t.Error("expected target-exists error")
	}
}

func TestParseAddress(t *testing.T) {
	cases := []struct{ in, mod, typ, name string }{
		{"aws_instance.web", "", "aws_instance", "web"},
		{"module.vpc.aws_subnet.private", "module.vpc", "aws_subnet", "private"},
		{"module.a.module.b.x.y", "module.a.module.b", "x", "y"},
		{"aws_instance.web[0]", "", "aws_instance", "web"},
	}
	for _, c := range cases {
		mod, typ, name, err := parseAddress(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if mod != c.mod || typ != c.typ || name != c.name {
			t.Errorf("%s -> (%q,%q,%q), want (%q,%q,%q)", c.in, mod, typ, name, c.mod, c.typ, c.name)
		}
	}
	if _, _, _, err := parseAddress("justone"); err == nil {
		t.Error("expected error for malformed address")
	}
}
