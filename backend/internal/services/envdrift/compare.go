package envdrift

import (
	"context"
	"sort"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/cloud/azure"
)

// Classification is the per-resource outcome of comparing a state resource to
// its live Azure counterpart.
type Classification string

const (
	// ClassPresent means the resource exists in Azure and its key properties
	// match the state — no drift.
	ClassPresent Classification = "present"
	// ClassMissing means the resource is recorded in state but absent in Azure.
	ClassMissing Classification = "missing"
	// ClassChanged means the resource exists in Azure but one or more key
	// properties differ from the state.
	ClassChanged Classification = "changed"
	// ClassUnknown means the resource could not be evaluated (credential
	// unavailable, access denied, or an unparseable ID).
	ClassUnknown Classification = "unknown"
)

// ResourceFinding is the comparison outcome for a single state resource.
type ResourceFinding struct {
	// Address is the Terraform address of the resource.
	Address string `json:"address"`
	// ARMID is the ARM resource ID that was looked up.
	ARMID string `json:"arm_id"`
	// Classification is the comparison outcome.
	Classification Classification `json:"classification"`
	// ChangedProps lists the names of key properties that differ, set only when
	// Classification is ClassChanged.
	ChangedProps []string `json:"changed_props,omitempty"`
	// Note carries a short reason for an unknown classification.
	Note string `json:"note,omitempty"`
}

// DriftChanges is the JSONB payload written to a drift_events row's changes
// column for environment drift. It mirrors the snapshot engine's added/removed/
// modified vocabulary (removed == missing in Azure, modified == changed in
// Azure) so existing drift-event consumers render it uniformly, while adding the
// detailed per-resource findings and present/unknown counts specific to
// environment drift.
type DriftChanges struct {
	// Removed lists the Terraform addresses of resources missing from Azure.
	Removed []string `json:"removed"`
	// Modified lists the Terraform addresses of resources whose key properties
	// changed in Azure.
	Modified []string `json:"modified"`
	// Added is always empty for environment drift (state is the source of truth;
	// resources only created in Azure are not represented in state). It is
	// retained for shape-compatibility with snapshot drift changes.
	Added []string `json:"added"`

	// PresentCount, MissingCount, ChangedCount, and UnknownCount summarise the
	// per-resource classifications across the whole state.
	PresentCount int `json:"present_count"`
	MissingCount int `json:"missing_count"`
	ChangedCount int `json:"changed_count"`
	UnknownCount int `json:"unknown_count"`

	// Findings holds the full per-resource detail for auditing.
	Findings []ResourceFinding `json:"findings"`
}

// HasDrift reports whether any resource was classified missing or changed.
// Unknown resources do not, on their own, constitute drift.
func (c *DriftChanges) HasDrift() bool {
	return c.MissingCount > 0 || c.ChangedCount > 0
}

// Compare looks up each azurerm resource ref via the reader, classifies it, and
// aggregates the results into a DriftChanges. The provided stateProps maps a
// resource's ARM ID to the key properties recorded in state, used to detect
// changed properties; pass nil to compare existence only. A non-nil error is
// returned only if the reader reports a transport-level failure, which aborts
// the whole comparison.
func Compare(
	ctx context.Context,
	reader azure.ResourceReader,
	refs []StateResourceRef,
	stateProps map[string]map[string]string,
) (*DriftChanges, error) {
	changes := &DriftChanges{
		Added:    []string{},
		Removed:  []string{},
		Modified: []string{},
		Findings: make([]ResourceFinding, 0, len(refs)),
	}

	for _, ref := range refs {
		state, err := reader.ReadResource(ctx, ref.ARMID)
		if err != nil {
			return nil, err
		}

		finding := classify(ref, state, stateProps[ref.ARMID])
		changes.Findings = append(changes.Findings, finding)

		switch finding.Classification {
		case ClassPresent:
			changes.PresentCount++
		case ClassMissing:
			changes.MissingCount++
			changes.Removed = append(changes.Removed, ref.Address)
		case ClassChanged:
			changes.ChangedCount++
			changes.Modified = append(changes.Modified, ref.Address)
		case ClassUnknown:
			changes.UnknownCount++
		}
	}

	sort.Strings(changes.Removed)
	sort.Strings(changes.Modified)
	return changes, nil
}

// classify turns a single ResourceState into a ResourceFinding, comparing key
// properties against the recorded state properties when the resource is present.
func classify(ref StateResourceRef, state azure.ResourceState, want map[string]string) ResourceFinding {
	finding := ResourceFinding{Address: ref.Address, ARMID: ref.ARMID}

	switch state.Existence {
	case azure.ExistenceMissing:
		finding.Classification = ClassMissing
	case azure.ExistenceUnknown:
		finding.Classification = ClassUnknown
		finding.Note = state.Note
	case azure.ExistencePresent:
		diff := diffProps(want, state.Properties)
		if len(diff) > 0 {
			finding.Classification = ClassChanged
			finding.ChangedProps = diff
		} else {
			finding.Classification = ClassPresent
		}
	default:
		finding.Classification = ClassUnknown
		finding.Note = "unrecognised existence"
	}
	return finding
}

// diffProps returns the sorted names of key properties present in both want and
// got whose values differ. Properties absent from either side are ignored so a
// resource is only flagged changed on a genuine value mismatch, never on
// missing data. It returns nil when want is empty (existence-only comparison).
func diffProps(want, got map[string]string) []string {
	if len(want) == 0 {
		return nil
	}
	var changed []string
	for key, wantVal := range want {
		gotVal, ok := got[key]
		if !ok {
			continue
		}
		if wantVal != gotVal {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}
