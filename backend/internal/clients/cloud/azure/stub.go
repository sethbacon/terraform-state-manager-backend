package azure

import (
	"context"
	"encoding/json"
	"fmt"
)

// StubReader is a fixture-backed ResourceReader. It serves pre-recorded ARM
// responses keyed by resource ID so the environment-drift engine can be tested
// and run in CI without any live Azure calls or credentials. Resource IDs not
// present in Responses are classified ExistenceMissing by default, modelling a
// resource that exists in state but not in the recorded environment; set
// DefaultMissing to false to classify unknown IDs as ExistenceUnknown instead.
type StubReader struct {
	// Responses maps an ARM resource ID to the state the reader should return.
	Responses map[string]ResourceState
	// DefaultMissing controls the classification for IDs absent from Responses.
	// When true (the zero value is set explicitly via the constructor), absent
	// IDs are reported ExistenceMissing; otherwise ExistenceUnknown.
	DefaultMissing bool
}

// NewStubReader returns a StubReader seeded with the given responses. Absent IDs
// default to ExistenceMissing, the common "deleted out of band" drift case.
func NewStubReader(responses map[string]ResourceState) *StubReader {
	return &StubReader{Responses: responses, DefaultMissing: true}
}

// ReadResource returns the recorded state for armID. An unparseable ID is
// classified ExistenceUnknown, mirroring the live reader. Absent IDs follow
// DefaultMissing. It never returns a transport error.
func (s *StubReader) ReadResource(_ context.Context, armID string) (ResourceState, error) {
	if _, err := ParseResourceID(armID); err != nil {
		return ResourceState{ID: armID, Existence: ExistenceUnknown, Note: "unparseable id"}, nil
	}
	if state, ok := s.Responses[armID]; ok {
		state.ID = armID
		return state, nil
	}
	if s.DefaultMissing {
		return ResourceState{ID: armID, Existence: ExistenceMissing}, nil
	}
	return ResourceState{ID: armID, Existence: ExistenceUnknown, Note: "no fixture"}, nil
}

// PresentFromARMJSON builds a present ResourceState from a recorded raw ARM GET
// response body, extracting the same key properties the live reader captures.
// It is a helper for loading JSON fixtures into a StubReader. It returns an
// error if the body is not valid JSON.
func PresentFromARMJSON(armID string, body []byte) (ResourceState, error) {
	var res armResource
	if err := json.Unmarshal(body, &res); err != nil {
		return ResourceState{}, fmt.Errorf("azure: decoding fixture for %s: %w", armID, err)
	}
	return ResourceState{
		ID:         armID,
		Existence:  ExistencePresent,
		Properties: ExtractKeyProperties(res.Location, res.Kind, skuName(res.SKU), res.Properties),
	}, nil
}
