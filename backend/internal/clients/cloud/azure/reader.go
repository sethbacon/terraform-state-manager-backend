// Package azure provides a read-only Azure Resource Manager (ARM) client used by
// the environment-drift engine to determine whether the live cloud resources
// backing a Terraform state still exist and still match the state.
//
// It is credential-agnostic: callers supply a Credential token source, so the
// development path can use a read-only service principal and the deployed path
// workload identity or a managed identity later, without this package changing.
// The package performs only ARM GET requests and never mutates Azure resources.
//
// Two ResourceReader implementations are provided: armReader, which issues live
// ARM GETs (constructed via NewARMReader), and StubReader, which serves recorded
// JSON fixtures so tests and CI make no network calls.
package azure

import (
	"context"
	"errors"
)

// ErrCredentialUnavailable is returned by a Credential when no token can be
// obtained — for example when no service principal, workload identity, or
// managed identity is configured. The environment-drift service treats this as
// an "unknown" classification for affected resources rather than a hard failure.
var ErrCredentialUnavailable = errors.New("azure: credential unavailable")

// Existence classifies the outcome of looking up a single ARM resource.
type Existence string

const (
	// ExistencePresent means the ARM GET returned 200 and the resource exists.
	ExistencePresent Existence = "present"
	// ExistenceMissing means the ARM GET returned 404 — the resource is recorded
	// in Terraform state but no longer exists in Azure.
	ExistenceMissing Existence = "missing"
	// ExistenceUnknown means existence could not be determined, e.g. the
	// credential was unavailable, access was denied, or the ID was not parseable.
	ExistenceUnknown Existence = "unknown"
)

// Credential is the token source the ARM reader consumes. Implementations return
// a bearer token scoped to the Azure Resource Manager audience
// (https://management.azure.com/.default). This slice deliberately does not
// implement live token acquisition; it only depends on this interface so the
// development service principal and deployed workload/managed identity can be
// supplied later. Returning ErrCredentialUnavailable signals "no credential
// configured" to callers.
type Credential interface {
	// Token returns a valid ARM bearer token, or an error. Implementations
	// should return ErrCredentialUnavailable when no credential is configured.
	Token(ctx context.Context) (string, error)
}

// CredentialFunc adapts a plain function to the Credential interface.
type CredentialFunc func(ctx context.Context) (string, error)

// Token implements Credential.
func (f CredentialFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// ResourceState is the read-only snapshot of a single ARM resource as observed
// by a ResourceReader. When Existence is ExistencePresent, Properties holds a
// small, comparable subset of the resource's observable attributes (location,
// SKU/kind, and the provisioning state) used to detect environment drift. The
// raw ARM response body is intentionally not retained.
type ResourceState struct {
	// ID is the ARM resource ID that was looked up.
	ID string `json:"id"`
	// Existence classifies whether the resource was found.
	Existence Existence `json:"existence"`
	// Properties holds the comparable key properties when the resource is
	// present; it is nil for missing or unknown resources.
	Properties map[string]string `json:"properties,omitempty"`
	// Note carries a short human-readable reason for an unknown classification
	// (e.g. "access denied", "unparseable id"); empty otherwise.
	Note string `json:"note,omitempty"`
}

// ResourceReader looks up the live state of an ARM resource by its resource ID.
// Implementations must not perform any write operations. A non-nil error is
// reserved for unexpected transport-level failures; ordinary outcomes
// (including 404 Not Found and access-denied) are reported through the returned
// ResourceState's Existence field so the comparison engine can classify every
// resource without aborting the run.
type ResourceReader interface {
	// ReadResource returns the observed state of the resource identified by the
	// ARM resource ID armID.
	ReadResource(ctx context.Context, armID string) (ResourceState, error)
}
