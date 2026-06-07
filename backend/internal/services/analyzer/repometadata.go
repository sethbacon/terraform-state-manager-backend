package analyzer

// RepoMetadata is the explicit repo-metadata input accepted by an analysis run.
//
// It carries the Terraform configuration signals needed to compute version
// pin-drift without performing any outbound git/ADO fetch — the caller supplies
// the raw file contents (or pre-parsed pins) directly. The live repo-fetch seam
// is deferred (O6); when added, it will populate this same struct before
// analysis.
type RepoMetadata struct {
	// RequiredVersionSpec is the raw `required_version` constraint string from
	// the repo's `terraform {}` block, when supplied directly by the caller.
	RequiredVersionSpec string `json:"required_version,omitempty"`
	// ConfigHCL is raw Terraform configuration HCL (e.g. the contents of the
	// `terraform {}` block or a versions.tf file). When present it is parsed to
	// derive RequiredVersionSpec (if not set explicitly) and module constraints.
	ConfigHCL string `json:"config_hcl,omitempty"`
	// LockFile is the raw text of a `.terraform.lock.hcl` file. When present it
	// is parsed into provider lock pins.
	LockFile string `json:"lock_file,omitempty"`
	// ProviderPins lets a caller supply provider pins directly instead of (or in
	// addition to) a raw LockFile.
	ProviderPins []ProviderLockPin `json:"provider_pins,omitempty"`
	// ModuleConstraints lets a caller supply module version constraints directly
	// instead of deriving them from ConfigHCL.
	ModuleConstraints []ModuleConstraint `json:"module_constraints,omitempty"`
}

// IsEmpty reports whether the metadata carries no usable signal.
func (m *RepoMetadata) IsEmpty() bool {
	if m == nil {
		return true
	}
	return m.RequiredVersionSpec == "" &&
		m.ConfigHCL == "" &&
		m.LockFile == "" &&
		len(m.ProviderPins) == 0 &&
		len(m.ModuleConstraints) == 0
}

// RepoMetadataAnalysis holds the parsed, structured result of ingesting
// RepoMetadata: the required_version spec, provider lock pins, and module
// version constraints. The version-drift report is computed separately, since
// it also needs the in-state terraform_version.
type RepoMetadataAnalysis struct {
	RequiredVersionSpec string             `json:"required_version_spec,omitempty"`
	ProviderLockPins    []ProviderLockPin  `json:"provider_lock_pins,omitempty"`
	ModuleConstraints   []ModuleConstraint `json:"module_constraints,omitempty"`
}

// AnalyzeRepoMetadata parses explicit repo metadata into structured fields.
//
// It is tolerant: a parse failure on one input (e.g. malformed lock file) does
// not abort the others — the corresponding field is simply left empty and the
// returned error aggregates what failed. The caller may treat a non-nil error
// as a soft warning and still persist whatever parsed successfully.
//
//   - required_version: taken from RequiredVersionSpec, else parsed from ConfigHCL.
//   - provider pins: ProviderPins, else parsed from LockFile.
//   - module constraints: ModuleConstraints, else parsed from ConfigHCL.
//
// PROVIDER PINS ARE STORED, NOT COMPARED: this slice records lock pins but does
// not compare them against actual in-state provider versions (deferred — no
// provider-version data source exists yet).
func AnalyzeRepoMetadata(meta *RepoMetadata) (*RepoMetadataAnalysis, error) {
	out := &RepoMetadataAnalysis{}
	if meta == nil {
		return out, nil
	}

	var firstErr error
	recordErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Parse the config HCL once if any field needs to be derived from it.
	var parsedConfig *TerraformConstraints
	needConfig := meta.ConfigHCL != "" &&
		(meta.RequiredVersionSpec == "" || len(meta.ModuleConstraints) == 0)
	if needConfig {
		tc, err := ExtractTerraformConstraints([]byte(meta.ConfigHCL), "")
		if err != nil {
			recordErr(err)
		} else {
			parsedConfig = tc
		}
	}

	// required_version: explicit wins, else from parsed config.
	out.RequiredVersionSpec = meta.RequiredVersionSpec
	if out.RequiredVersionSpec == "" && parsedConfig != nil {
		out.RequiredVersionSpec = parsedConfig.RequiredVersion
	}

	// module constraints: explicit wins, else from parsed config.
	if len(meta.ModuleConstraints) > 0 {
		out.ModuleConstraints = meta.ModuleConstraints
	} else if parsedConfig != nil {
		out.ModuleConstraints = parsedConfig.ModuleConstraints
	}

	// provider pins: explicit wins, else from lock file.
	if len(meta.ProviderPins) > 0 {
		out.ProviderLockPins = meta.ProviderPins
	} else if meta.LockFile != "" {
		pins, err := ParseLockFile([]byte(meta.LockFile))
		if err != nil {
			recordErr(err)
		} else {
			out.ProviderLockPins = SortedLockPins(pins)
		}
	}

	return out, firstErr
}
