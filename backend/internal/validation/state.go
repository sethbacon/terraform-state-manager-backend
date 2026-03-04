package validation

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// MinStateVersion is the minimum Terraform state format version supported.
const MinStateVersion = 3

// RequiredFields lists the top-level fields that every valid state file must contain.
var RequiredFields = []string{"version", "resources"}

// SensitiveResourceFields lists attribute key names that indicate a resource
// instance holds sensitive data such as credentials or secrets.
var SensitiveResourceFields = []string{
	"password", "secret", "private_key", "token", "api_key",
}

// ValidationResult holds the result of state file validation.
type ValidationResult struct {
	IsValid  bool     `json:"is_valid"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// newResult creates an empty, initially-valid ValidationResult.
func newResult() *ValidationResult {
	return &ValidationResult{
		IsValid:  true,
		Errors:   []string{},
		Warnings: []string{},
	}
}

// addError records an error and marks the result as invalid.
func (r *ValidationResult) addError(msg string) {
	r.Errors = append(r.Errors, msg)
	r.IsValid = false
}

// addWarning records a non-fatal warning.
func (r *ValidationResult) addWarning(msg string) {
	r.Warnings = append(r.Warnings, msg)
}

// ValidateStateFile validates a parsed state file for correctness.
//
// Checks performed:
//  1. Version must be >= MinStateVersion
//  2. Resources must not be nil
//  3. Each resource must have a type and provider
//  4. Warns if terraform_version is empty
//  5. Warns if serial is 0
func ValidateStateFile(state *hcp.StateFile) *ValidationResult {
	result := newResult()

	if state == nil {
		result.addError("state file is nil")
		return result
	}

	// 1. Version check
	if state.Version < MinStateVersion {
		result.addError(fmt.Sprintf(
			"state version %d is below minimum supported version %d",
			state.Version, MinStateVersion))
	}

	// 2. Resources must be present
	if state.Resources == nil {
		result.addError("state file has no resources field")
	} else {
		// 3. Validate each resource has required fields
		for i, res := range state.Resources {
			if res.Type == "" {
				result.addError(fmt.Sprintf("resource[%d] is missing a type", i))
			}
			if res.Provider == "" {
				result.addError(fmt.Sprintf("resource[%d] (%s) is missing a provider", i, res.Type))
			}
		}
	}

	// 4. Warn on empty terraform_version
	if state.TerraformVersion == "" {
		result.addWarning("terraform_version is empty; unable to determine Terraform CLI version")
	}

	// 5. Warn on serial == 0
	if state.Serial == 0 {
		result.addWarning("state serial is 0; this may indicate an uninitialized state")
	}

	return result
}

// ValidateStateBytes validates raw state JSON bytes by unmarshaling them into
// an hcp.StateFile and running ValidateStateFile. Parse errors are reported as
// validation errors.
func ValidateStateBytes(data []byte) *ValidationResult {
	if len(data) == 0 {
		result := newResult()
		result.addError("state data is empty")
		return result
	}

	var state hcp.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		result := newResult()
		result.addError(fmt.Sprintf("failed to parse state JSON: %v", err))
		return result
	}

	return ValidateStateFile(&state)
}

// CheckSensitiveAttributes checks whether any instance in the given resource
// contains attributes whose names match known sensitive patterns (passwords,
// secrets, tokens, etc.). It returns true if at least one sensitive attribute
// key is found.
func CheckSensitiveAttributes(resource *hcp.StateResource) bool {
	if resource == nil {
		return false
	}

	for _, inst := range resource.Instances {
		if len(inst.Attributes) == 0 {
			continue
		}

		// Unmarshal the raw attributes into a generic map to inspect keys.
		var attrs map[string]interface{}
		if err := json.Unmarshal(inst.Attributes, &attrs); err != nil {
			continue
		}

		for key := range attrs {
			lower := strings.ToLower(key)
			for _, sensitive := range SensitiveResourceFields {
				if strings.Contains(lower, sensitive) {
					return true
				}
			}
		}

		// Also check the SensitiveAttributes marker
		if len(inst.SensitiveAttributes) > 0 && string(inst.SensitiveAttributes) != "null" && string(inst.SensitiveAttributes) != "[]" {
			return true
		}
	}

	return false
}
