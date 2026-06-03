package docs

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSwaggerJSONEmbedded(t *testing.T) {
	if len(SwaggerJSON) == 0 {
		t.Fatal("SwaggerJSON is empty — swagger.json was not embedded")
	}
}

func TestSwaggerYAMLIsValidYAML(t *testing.T) {
	if len(SwaggerYAML) == 0 {
		t.Fatal("SwaggerYAML is empty")
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(SwaggerYAML, &doc); err != nil {
		t.Fatalf("SwaggerYAML is not valid YAML: %v", err)
	}
	if _, ok := doc["swagger"]; !ok {
		if _, ok := doc["openapi"]; !ok {
			t.Error(`SwaggerYAML is missing the "swagger"/"openapi" version key`)
		}
	}
}

func TestJSONToYAMLFallsBackOnInvalidInput(t *testing.T) {
	// Unparseable input must fall back to the original bytes rather than panic
	// or return empty.
	bad := []byte("[1, 2") // unterminated flow sequence — invalid YAML/JSON
	if got := jsonToYAML(bad); string(got) != string(bad) {
		t.Errorf("expected fallback to original bytes on parse failure, got %q", got)
	}
}
