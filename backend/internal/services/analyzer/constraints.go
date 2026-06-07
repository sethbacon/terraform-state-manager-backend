package analyzer

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// ModuleConstraint captures a module call's source and version constraint as
// declared in a `module "name" { ... }` block.
type ModuleConstraint struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
}

// TerraformConstraints holds the version-related declarations extracted from a
// Terraform configuration's HCL source: the root `required_version` constraint
// from the `terraform {}` block plus any `module` call version constraints.
type TerraformConstraints struct {
	// RequiredVersion is the `terraform { required_version = "..." }` value.
	// Empty when the configuration declares no constraint.
	RequiredVersion string `json:"required_version,omitempty"`
	// ModuleConstraints lists every `module` block that declares a version.
	ModuleConstraints []ModuleConstraint `json:"module_constraints,omitempty"`
}

// ExtractTerraformConstraints parses Terraform configuration HCL and extracts
// the root `required_version` constraint and module version constraints.
//
// It uses the native HCL syntax parser (schema-less) so arbitrary configuration
// can be ingested without modelling the full Terraform language. Only the
// `terraform` and `module` top-level blocks are inspected; everything else is
// ignored. A parse error is returned only when the input is not valid HCL.
func ExtractTerraformConstraints(src []byte, filename string) (*TerraformConstraints, error) {
	if filename == "" {
		filename = "config.tf"
	}

	file, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse terraform config %q: %s", filename, diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected HCL body type for %q", filename)
	}

	constraints := &TerraformConstraints{}

	for _, block := range body.Blocks {
		switch block.Type {
		case "terraform":
			if v, ok := stringAttr(block.Body, "required_version"); ok {
				constraints.RequiredVersion = v
			}
		case "module":
			mc := ModuleConstraint{}
			if len(block.Labels) > 0 {
				mc.Name = block.Labels[0]
			}
			if v, ok := stringAttr(block.Body, "source"); ok {
				mc.Source = v
			}
			if v, ok := stringAttr(block.Body, "version"); ok {
				mc.Version = v
			}
			// Only record module blocks that declare a version constraint;
			// versionless local-path modules carry no pin-drift signal.
			if mc.Version != "" {
				constraints.ModuleConstraints = append(constraints.ModuleConstraints, mc)
			}
		}
	}

	return constraints, nil
}

// stringAttr evaluates the named attribute on a block body and returns its
// string value. It returns ok=false when the attribute is absent or does not
// evaluate to a literal string (e.g. it references a variable). Evaluation uses
// a nil context, so only constant literal expressions resolve — which is exactly
// the case for version constraints in practice.
func stringAttr(body *hclsyntax.Body, name string) (string, bool) {
	attr, ok := body.Attributes[name]
	if !ok {
		return "", false
	}
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || val.IsNull() || val.Type() != cty.String {
		return "", false
	}
	return val.AsString(), true
}
