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
	reWorkingDir = regexp.MustCompile(`^[A-Za-z0-9._/\-]*$`)
	// reStateKeyForbidden rejects the characters that would let a state key
	// break out of a CI template parameter or a shell/YAML context (control
	// characters, quotes, backtick, `$`, and shell metacharacters). It is a
	// denylist rather than reWorkingDir's allowlist because backend keys are
	// legitimately looser than directory names (`env=prod/app.tfstate`).
	reStateKeyForbidden = regexp.MustCompile("[\x00-\x1f\x7f\"'`$;|&<>\\\\]")
	reGitRef            = regexp.MustCompile(`^[A-Za-z0-9._/\-]*$`)
	reTFVersion         = regexp.MustCompile(`^[A-Za-z0-9._\-]*$`)
	reHost              = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$`)
	reProviderName      = regexp.MustCompile(`^[a-z0-9_\-]+$`)
	reVersionSpec       = regexp.MustCompile(`^[A-Za-z0-9 .,<>=~!^*\-]+$`)
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

// maxDriftTargets bounds how many targets a single fan-out dispatch may carry
// (drift-fleet-scale.md Phase 1, task 1.3).
const maxDriftTargets = 100

// maxStateKeyLen bounds one target's state_key; real keys are a few dozen
// characters, and the whole targets list has to fit an ADO template parameter.
const maxStateKeyLen = 512

// validateDriftTargets is the write-time validation for a dispatch's
// items() -- called from CreateRun and scheduleRequest.validate() so a
// malformed request or schedule is refused before it can ever reach dispatch,
// covering the legacy single-target shape and a fan-out request alike (both
// arrive through items()).
//
// It bounds the count, runs each item's working_dir through the same
// shell/HCL-hostile-character check every other dispatch input gets, and
// refuses a (source_id, state_key) pair that repeats within the request: two
// targets naming the same state would race to record the same detection, and
// UpsertDetection/ResolveClean are last-write-wins (see dispatchDriftBatch), so
// catching the collision here is strictly better than letting one target's
// result silently overwrite the other's. A pair that is BOTH empty carries no
// detection identity to collide on, so it is exempt -- several untracked
// targets (no source_id/state_key, only a working_dir) are not duplicates of
// each other.
func validateDriftTargets(items []DriftTargetItem) error {
	if len(items) > maxDriftTargets {
		return fmt.Errorf("too many targets (%d); the maximum is %d", len(items), maxDriftTargets)
	}
	seen := make(map[[2]string]bool, len(items))
	for _, item := range items {
		if err := validatePipelineInputs(item.WorkingDir, "", "", "", nil, nil); err != nil {
			return err
		}
		// state_key travels into the "targets" CI template parameter under
		// fan-out (and into the backend key the template derives from it), so
		// it gets the same hostile-character screen working_dir already has.
		if len(item.StateKey) > maxStateKeyLen {
			return fmt.Errorf("state_key too long (%d); the maximum is %d", len(item.StateKey), maxStateKeyLen)
		}
		if reStateKeyForbidden.MatchString(item.StateKey) {
			return fmt.Errorf("state_key %q contains a character that is not allowed in a CI parameter", item.StateKey)
		}
		if item.SourceID == "" && item.StateKey == "" {
			continue
		}
		key := [2]string{item.SourceID, item.StateKey}
		if seen[key] {
			return fmt.Errorf("duplicate target (source_id=%q, state_key=%q)", item.SourceID, item.StateKey)
		}
		seen[key] = true
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
