package analyzer

import "testing"

// resourceKeyState mixes a for_each resource (string keys), a count resource
// (integer keys), and an un-indexed singleton.
const resourceKeyState = `{
  "version": 4,
  "resources": [
    {"module":"module.m","mode":"managed","type":"aws_prefix_list","name":"this","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","each":"map",
     "instances":[{"index_key":"a"},{"index_key":"b"}]},
    {"mode":"managed","type":"aws_instance","name":"web","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]",
     "instances":[{"index_key":0},{"index_key":1},{"index_key":2}]},
    {"mode":"managed","type":"random_id","name":"suffix","provider":"provider[\"registry.terraform.io/hashicorp/random\"]",
     "instances":[{}]}
  ]
}`

func TestListResourcesInstanceKeys(t *testing.T) {
	got, err := ListResources([]byte(resourceKeyState))
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	byName := map[string]ResourceSummary{}
	for _, r := range got {
		byName[r.Name] = r
	}

	// for_each keys come back as their string values, in state order.
	fe := byName["this"]
	if len(fe.InstanceKeys) != 2 || fe.InstanceKeys[0] != "a" || fe.InstanceKeys[1] != "b" {
		t.Errorf("for_each keys = %v, want [a b]", fe.InstanceKeys)
	}

	// count keys come back as JSON numbers (float64).
	ct := byName["web"]
	if len(ct.InstanceKeys) != 3 {
		t.Fatalf("count keys = %v, want 3 entries", ct.InstanceKeys)
	}
	if n, ok := ct.InstanceKeys[0].(float64); !ok || n != 0 {
		t.Errorf("count key[0] = %v, want 0", ct.InstanceKeys[0])
	}

	// An un-indexed singleton exposes no instance keys.
	if s := byName["suffix"]; s.InstanceKeys != nil {
		t.Errorf("singleton InstanceKeys = %v, want nil", s.InstanceKeys)
	}
}
