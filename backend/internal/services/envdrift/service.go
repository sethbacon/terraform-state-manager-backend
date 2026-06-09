package envdrift

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/cloud/azure"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// driftEventCreator is the slice of the drift-event repository this service
// needs. Depending on the interface rather than the concrete repository keeps
// the service unit-testable without a database.
type driftEventCreator interface {
	Create(ctx context.Context, event *models.DriftEvent) error
}

// Service detects environment drift for a workspace by comparing its Terraform
// state's azurerm resources against live Azure and persisting a drift_events
// row when drift is found.
type Service struct {
	reader    azure.ResourceReader
	driftRepo driftEventCreator
}

// NewService creates an environment-drift Service from an Azure ResourceReader
// (live or fixture-backed) and a drift-event repository.
func NewService(reader azure.ResourceReader, driftRepo *repositories.DriftEventRepository) *Service {
	return &Service{reader: reader, driftRepo: driftRepo}
}

// Result is the outcome of a single environment-drift detection run.
type Result struct {
	// Changes is the aggregated comparison result.
	Changes *DriftChanges
	// Severity is the classified drift severity (info/warning/critical).
	Severity string
	// DriftEventID is the ID of the persisted drift_events row, empty when no
	// drift was detected and therefore no event was written.
	DriftEventID string
}

// DetectForState is the engine entry point a scheduler or HTTP trigger will call
// in a later slice. It extracts the azurerm resources from the parsed state,
// compares each against live Azure via the configured ResourceReader, and — when
// any resource is missing or changed — writes a drift_events row with
// drift_source = "environment". stateProps optionally supplies per-ARM-ID key
// properties recorded in state for change detection; pass nil for an
// existence-only comparison.
//
// When no drift is detected, no event is written and Result.DriftEventID is
// empty; the populated Changes is still returned so callers can record metrics.
func (s *Service) DetectForState(
	ctx context.Context,
	orgID string,
	workspaceName string,
	state *hcp.StateFile,
	stateProps map[string]map[string]string,
) (*Result, error) {
	logger := slog.With(
		"component", "envdrift_service",
		"org_id", orgID,
		"workspace", workspaceName,
	)

	refs := ExtractAzureResources(state)

	changes, err := Compare(ctx, s.reader, refs, stateProps)
	if err != nil {
		return nil, fmt.Errorf("envdrift: comparing state against azure: %w", err)
	}

	severity := models.ClassifyDriftSeverity(
		len(changes.Added),
		changes.MissingCount,
		changes.ChangedCount,
	)
	result := &Result{Changes: changes, Severity: severity}

	if !changes.HasDrift() {
		logger.Info("No environment drift detected",
			"resources", len(refs),
			"present", changes.PresentCount,
			"unknown", changes.UnknownCount)
		return result, nil
	}

	changesBytes, err := json.Marshal(changes)
	if err != nil {
		return nil, fmt.Errorf("envdrift: marshalling drift changes: %w", err)
	}

	event := &models.DriftEvent{
		OrganizationID: orgID,
		WorkspaceName:  workspaceName,
		Changes:        changesBytes,
		Severity:       severity,
		DriftSource:    models.DriftSourceEnvironment,
	}
	if err := s.driftRepo.Create(ctx, event); err != nil {
		return nil, fmt.Errorf("envdrift: creating drift event: %w", err)
	}
	result.DriftEventID = event.ID

	logger.Info("Environment drift detected",
		"severity", severity,
		"missing", changes.MissingCount,
		"changed", changes.ChangedCount,
		"present", changes.PresentCount,
		"unknown", changes.UnknownCount)

	return result, nil
}
