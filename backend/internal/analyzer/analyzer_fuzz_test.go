package analyzer

import "testing"

// minimalState is a small valid Terraform state (format v4) seed.
const minimalState = `{
  "version": 4,
  "terraform_version": "1.9.5",
  "serial": 7,
  "lineage": "abc-123",
  "resources": [
    {
      "module": "",
      "mode": "managed",
      "type": "aws_instance",
      "name": "web",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [{}, {}]
    },
    {
      "mode": "data",
      "type": "aws_ami",
      "name": "ubuntu",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [{}]
    }
  ]
}`

// FuzzAnalyze ensures the state parser never panics on arbitrary bytes — only
// returns an error or a well-formed Analysis.
func FuzzAnalyze(f *testing.F) {
	f.Add([]byte(minimalState))
	f.Add([]byte{})
	f.Add([]byte("not json"))
	f.Add([]byte(`{"version": "four"}`))
	f.Add([]byte(`{"resources": [{"instances": "nope"}]}`))
	f.Add([]byte(`{"resources": null}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := Analyze(data)
		if err != nil {
			return // malformed input is expected to error, never panic
		}
		if a == nil {
			t.Fatal("Analyze returned nil, nil")
		}
		if a.RUM < 0 || a.ManagedResources < 0 || a.TotalResources < 0 {
			t.Fatalf("negative counts: %+v", a)
		}
		if a.RUM > a.ManagedResources {
			t.Fatalf("RUM (%d) exceeds managed resources (%d)", a.RUM, a.ManagedResources)
		}
	})
}

// FuzzListResources mirrors FuzzAnalyze for the resource-list view.
func FuzzListResources(f *testing.F) {
	f.Add([]byte(minimalState))
	f.Add([]byte{})
	f.Add([]byte(`{"resources": [{}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		rs, err := ListResources(data)
		if err != nil {
			return
		}
		for _, r := range rs {
			if r.Instances < 0 {
				t.Fatalf("negative instance count: %+v", r)
			}
		}
	})
}
