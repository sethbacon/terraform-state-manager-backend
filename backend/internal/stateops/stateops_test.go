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

// forEachState has a for_each resource (string keys a/b/c) and a count resource
// (indexes 0/1) so instance-scoped rm/mv can be exercised.
const forEachState = `{
  "version": 4,
  "serial": 2,
  "resources": [
    {"module":"module.m","mode":"managed","type":"aws_prefix_list","name":"this","provider":"p","each":"map",
     "instances":[
       {"index_key":"a","attributes":{"id":"a"}},
       {"index_key":"b","attributes":{"id":"b"}},
       {"index_key":"c","attributes":{"id":"c"}}
     ]},
    {"mode":"managed","type":"aws_instance","name":"c","provider":"p",
     "instances":[{"index_key":0,"attributes":{}},{"index_key":1,"attributes":{}}]}
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
	cases := []struct {
		in, mod, typ, name string
		index              any
	}{
		{"aws_instance.web", "", "aws_instance", "web", nil},
		{"module.vpc.aws_subnet.private", "module.vpc", "aws_subnet", "private", nil},
		{"module.a.module.b.x.y", "module.a.module.b", "x", "y", nil},
		{"aws_instance.web[0]", "", "aws_instance", "web", 0},
		{`aws_instance.web["a"]`, "", "aws_instance", "web", "a"},
		{`module.vpc.aws_subnet.private["b.c"]`, "module.vpc", "aws_subnet", "private", "b.c"},
	}
	for _, c := range cases {
		mod, typ, name, index, err := parseAddress(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if mod != c.mod || typ != c.typ || name != c.name || index != c.index {
			t.Errorf("%s -> (%q,%q,%q,%v), want (%q,%q,%q,%v)", c.in, mod, typ, name, index, c.mod, c.typ, c.name, c.index)
		}
	}
	if _, _, _, _, err := parseAddress("justone"); err == nil {
		t.Error("expected error for malformed address")
	}
	if _, _, _, _, err := parseAddress(`aws_instance.web["bad`); err == nil {
		t.Error("expected error for malformed index")
	}
}

func TestRemoveForEachInstance(t *testing.T) {
	out, err := RemoveResource([]byte(forEachState), `module.m.aws_prefix_list.this["a"]`)
	if err != nil {
		t.Fatalf("RemoveResource instance: %v", err)
	}
	res := parse(t, out)["resources"].([]any)
	if len(res) != 2 {
		t.Fatalf("expected both resource blocks kept, got %d", len(res))
	}
	for _, r := range res {
		rm := r.(map[string]any)
		if rm["type"] != "aws_prefix_list" {
			continue
		}
		insts := rm["instances"].([]any)
		if len(insts) != 2 {
			t.Fatalf("expected 2 instances left, got %d", len(insts))
		}
		for _, in := range insts {
			if in.(map[string]any)["index_key"] == "a" {
				t.Error("instance [\"a\"] should have been removed")
			}
		}
	}
}

func TestRemoveCountInstance(t *testing.T) {
	out, err := RemoveResource([]byte(forEachState), "aws_instance.c[0]")
	if err != nil {
		t.Fatalf("RemoveResource count instance: %v", err)
	}
	for _, r := range parse(t, out)["resources"].([]any) {
		rm := r.(map[string]any)
		if rm["type"] != "aws_instance" {
			continue
		}
		insts := rm["instances"].([]any)
		if len(insts) != 1 || insts[0].(map[string]any)["index_key"].(float64) != 1 {
			t.Errorf("expected only index 1 to remain, got %v", insts)
		}
	}
}

func TestRemoveInstanceDropsEmptyBlock(t *testing.T) {
	out, err := RemoveResource([]byte(forEachState), "aws_instance.c[0]")
	if err != nil {
		t.Fatalf("rm [0]: %v", err)
	}
	out, err = RemoveResource(out, "aws_instance.c[1]")
	if err != nil {
		t.Fatalf("rm [1]: %v", err)
	}
	for _, r := range parse(t, out)["resources"].([]any) {
		if r.(map[string]any)["type"] == "aws_instance" {
			t.Error("empty count resource block should have been dropped")
		}
	}
}

func TestRemoveInstanceNotFound(t *testing.T) {
	if _, err := RemoveResource([]byte(forEachState), `module.m.aws_prefix_list.this["z"]`); err == nil {
		t.Error("expected not-found for a missing instance key")
	}
}

func TestMoveInstanceRekey(t *testing.T) {
	out, err := MoveResource([]byte(forEachState), `module.m.aws_prefix_list.this["a"]`, `module.m.aws_prefix_list.this["z"]`)
	if err != nil {
		t.Fatalf("mv rekey: %v", err)
	}
	res := parse(t, out)["resources"].([]any)
	if len(res) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(res))
	}
	for _, r := range res {
		rm := r.(map[string]any)
		if rm["type"] != "aws_prefix_list" {
			continue
		}
		insts := rm["instances"].([]any)
		if len(insts) != 3 {
			t.Fatalf("expected 3 instances after rekey, got %d", len(insts))
		}
		keys := map[any]bool{}
		for _, in := range insts {
			keys[in.(map[string]any)["index_key"]] = true
		}
		if keys["a"] {
			t.Error("old key a still present after rekey")
		}
		if !keys["z"] {
			t.Error("new key z missing after rekey")
		}
	}
}

func TestMoveInstanceToNewResource(t *testing.T) {
	out, err := MoveResource([]byte(forEachState), "aws_instance.c[0]", "aws_instance.d[0]")
	if err != nil {
		t.Fatalf("mv to new: %v", err)
	}
	var foundC, foundD bool
	for _, r := range parse(t, out)["resources"].([]any) {
		rm := r.(map[string]any)
		if rm["type"] != "aws_instance" {
			continue
		}
		switch rm["name"] {
		case "d":
			foundD = true
			if len(rm["instances"].([]any)) != 1 {
				t.Error("new resource d should have 1 instance")
			}
		case "c":
			foundC = true
			if len(rm["instances"].([]any)) != 1 {
				t.Error("source c should have 1 instance left")
			}
		}
	}
	if !foundD {
		t.Error("new resource d not created")
	}
	if !foundC {
		t.Error("source c should remain with its other instance")
	}
}

func TestMoveInstanceRequiresIndexedTarget(t *testing.T) {
	if _, err := MoveResource([]byte(forEachState), "aws_instance.c[0]", "aws_instance.d"); err == nil {
		t.Error("expected error moving an instance to a non-indexed target")
	}
}

func TestMoveInstanceClash(t *testing.T) {
	if _, err := MoveResource([]byte(forEachState), `module.m.aws_prefix_list.this["a"]`, `module.m.aws_prefix_list.this["b"]`); err == nil {
		t.Error("expected clash error for an existing destination instance")
	}
}
