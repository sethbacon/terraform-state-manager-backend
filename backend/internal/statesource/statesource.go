// Package statesource abstracts the backends where Terraform state already lives
// (local files, HCP/TFC, Azure Blob, S3, GCS, Git, Consul, PostgreSQL, Kubernetes,
// and the generic http backend) behind a single read/write interface with
// optional advisory locking.
package statesource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// ErrNotFound marks Read failures where the state genuinely does not exist on
// the backend, as opposed to a transient backend failure. Guarded write paths
// rely on this distinction: a missing state is safe to treat as a first write,
// but a transient read error must abort (otherwise the pre-write backup and
// serial/lineage checks would be silently skipped).
var ErrNotFound = errors.New("not found")

// IsNotFound reports whether err means "the state does not exist" rather than
// "the backend failed". Filesystem-backed connectors (local, git) surface
// fs.ErrNotExist; the rest wrap ErrNotFound explicitly.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// StateRef identifies one state file within a source (e.g. a relative path for
// local, a workspace/key for remote backends).
type StateRef struct {
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified,omitempty"`
	// Version is an opaque backend change token (consul ModifyIndex, pg
	// content hash) for backends whose listings carry no timestamp; it
	// strengthens sync change-detection where size alone is ambiguous.
	Version string `json:"version,omitempty"`
}

// RawState is the bytes of a state file plus its metadata.
type RawState struct {
	Key          string     `json:"key"`
	Data         []byte     `json:"-"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified,omitempty"`
}

// Connector reads and writes state on a configured backend.
type Connector interface {
	// List enumerates the state files available under the source.
	List(ctx context.Context) ([]StateRef, error)
	// Read fetches the raw bytes of the state identified by key.
	Read(ctx context.Context, key string) (*RawState, error)
	// Write replaces the state identified by key with data. Backends that do not
	// support writes return an error (callers must back up before writing).
	Write(ctx context.Context, key string, data []byte) error
	// Delete removes the state object identified by key. A missing object is
	// reported as ErrNotFound. Backends that cannot remove state in place (e.g.
	// HCP/TFC, whose state versions are immutable) return an error; callers must
	// back up before deleting.
	Delete(ctx context.Context, key string) error
}

// Locker is optionally implemented by connectors that support advisory locking of
// a state key. The edit pipeline acquires a lock before mutating when available;
// connectors that don't implement it fall back to backup-based recoverability.
type Locker interface {
	Lock(ctx context.Context, key string) (lockID string, err error)
	Unlock(ctx context.Context, key, lockID string) error
}

// New builds a connector for the given source type, its (non-secret) config, and
// its decrypted credentials (may be nil/empty when none are required, e.g. local).
func New(sourceType string, config map[string]any, credentials map[string]any) (Connector, error) {
	switch sourceType {
	case "local":
		return newLocal(config)
	case "hcp":
		return newHCP(config, credentials)
	case "s3":
		return newS3(config, credentials)
	case "azureblob":
		return newAzure(config, credentials)
	case "gcs":
		return newGCS(config, credentials)
	case "git":
		return newGit(config, credentials)
	case "consul":
		return newConsul(config, credentials)
	case "pg":
		return newPG(config, credentials)
	case "kubernetes":
		return newK8s(config, credentials)
	case "http":
		return newHTTPBackend(config, credentials)
	default:
		return nil, fmt.Errorf("unknown state source type %q", sourceType)
	}
}
