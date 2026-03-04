package snapshot

import (
	"encoding/json"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// DriftChanges describes the differences detected between two state snapshots.
// It lists resource types that were added, removed, or modified, along with
// the net change in total resource count.
type DriftChanges struct {
	Added         []string `json:"added"`
	Removed       []string `json:"removed"`
	Modified      []string `json:"modified"`
	ResourceDelta int      `json:"resource_delta"`
}

// HasChanges returns true if any additions, removals, or modifications were detected.
func (dc *DriftChanges) HasChanges() bool {
	return len(dc.Added) > 0 || len(dc.Removed) > 0 || len(dc.Modified) > 0
}

// DetectDrift compares the snapshot_data JSONB of two snapshots to identify
// resource type changes. It parses the stored SnapshotData, extracts the
// resource_types map from each, and computes:
//   - Added:   resource types present in 'after' but not in 'before'
//   - Removed: resource types present in 'before' but not in 'after'
//   - Modified: resource types present in both but with different counts
//   - ResourceDelta: after.ResourceCount - before.ResourceCount
func DetectDrift(before, after *models.StateSnapshot) *DriftChanges {
	if before == nil || after == nil {
		return nil
	}

	beforeData := parseSnapshotData(before.SnapshotData)
	afterData := parseSnapshotData(after.SnapshotData)

	beforeTypes := beforeData.ResourceTypes
	afterTypes := afterData.ResourceTypes

	if beforeTypes == nil {
		beforeTypes = make(map[string]int)
	}
	if afterTypes == nil {
		afterTypes = make(map[string]int)
	}

	changes := &DriftChanges{
		Added:         make([]string, 0),
		Removed:       make([]string, 0),
		Modified:      make([]string, 0),
		ResourceDelta: after.ResourceCount - before.ResourceCount,
	}

	// Find added and modified resource types.
	for resType, afterCount := range afterTypes {
		beforeCount, exists := beforeTypes[resType]
		if !exists {
			changes.Added = append(changes.Added, resType)
		} else if beforeCount != afterCount {
			changes.Modified = append(changes.Modified, resType)
		}
	}

	// Find removed resource types.
	for resType := range beforeTypes {
		if _, exists := afterTypes[resType]; !exists {
			changes.Removed = append(changes.Removed, resType)
		}
	}

	return changes
}

// parseSnapshotData unmarshals a json.RawMessage into a SnapshotData struct.
// If parsing fails, it returns an empty SnapshotData with an initialized map.
func parseSnapshotData(raw json.RawMessage) models.SnapshotData {
	var data models.SnapshotData
	if err := json.Unmarshal(raw, &data); err != nil {
		return models.SnapshotData{
			ResourceTypes: make(map[string]int),
		}
	}
	if data.ResourceTypes == nil {
		data.ResourceTypes = make(map[string]int)
	}
	return data
}
