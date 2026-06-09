package api

import (
	"fmt"
	"regexp"
)

// Input validators for values that are forwarded to CI runners as workflow
// inputs/parameters. Even though the workflow templates pass these via env vars
// (not inline shell), we reject shell/HCL-hostile characters server-side as
// defense-in-depth before dispatch. See the CI-injection findings.
var (
	reWorkingDir   = regexp.MustCompile(`^[A-Za-z0-9._/\-]*$`)
	reGitRef       = regexp.MustCompile(`^[A-Za-z0-9._/\-]*$`)
	reTFVersion    = regexp.MustCompile(`^[A-Za-z0-9._\-]*$`)
	reHost         = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$`)
	reProviderName = regexp.MustCompile(`^[a-z0-9_\-]+$`)
	reVersionSpec  = regexp.MustCompile(`^[A-Za-z0-9 .,<>=~!^*\-]+$`)
)

// validatePipelineInputs rejects untrusted run inputs that contain characters
// unsafe for the CI shell / generated HCL. Empty optional fields are allowed.
func validatePipelineInputs(workingDir, repoRef, tfVersion, registryHost string, providerVersions, moduleVersions map[string]string) error {
	if workingDir != "" && !reWorkingDir.MatchString(workingDir) {
		return fmt.Errorf("invalid working_dir (allowed: letters, digits, . _ / -)")
	}
	if repoRef != "" && !reGitRef.MatchString(repoRef) {
		return fmt.Errorf("invalid repo_ref (allowed: letters, digits, . _ / -)")
	}
	if tfVersion != "" && !reTFVersion.MatchString(tfVersion) {
		return fmt.Errorf("invalid terraform_version")
	}
	if registryHost != "" && !reHost.MatchString(registryHost) {
		return fmt.Errorf("invalid registry_host (expected host or host:port)")
	}
	if err := validateVersionMap("provider", providerVersions); err != nil {
		return err
	}
	if err := validateVersionMap("module", moduleVersions); err != nil {
		return err
	}
	return nil
}

func validateVersionMap(kind string, m map[string]string) error {
	for name, version := range m {
		if !reProviderName.MatchString(name) {
			return fmt.Errorf("invalid %s name %q (allowed: lowercase letters, digits, _ -)", kind, name)
		}
		if !reVersionSpec.MatchString(version) {
			return fmt.Errorf("invalid %s version constraint for %q", kind, name)
		}
	}
	return nil
}
