<!-- markdownlint-disable MD013 -->
# 2. Fail-Closed State Writes

**Status**: Accepted

## Context

The State Manager edits live Terraform state where it already lives — replacing a raw state file, applying `terraform state rm`/`mv` operations, and restoring a prior backup. These are among the most destructive operations the system can perform: a botched write can corrupt the source of truth for an entire environment.

The edit pipeline is **validate → backup → write → audit** (`internal/api/edit.go`). Two of those steps depend on first **reading the current state**: the pre-write backup (so every edit is one-click reversible) and the serial/lineage guard (so a stale write does not silently regress a state that advanced underneath the editor).

The read can fail two fundamentally different ways:

1. **The state genuinely does not exist** — this is the *first write* to a new key. There is nothing to back up and no prior serial to compare against; proceeding is correct.
2. **The backend failed transiently** — a 500 from S3, a dropped Consul connection, an HCP timeout. Here the current state may very well exist; we just could not read it.

A naive implementation treats every read error the same and proceeds when the read returns no data. That is exactly backwards: it would skip the backup and the serial check **precisely when the backend is flaky** — the worst possible moment to write blind. The distinction between "not found" and "backend failed" must be load-bearing.

## Decision

Distinguish "not found" from "backend error" at the connector boundary and **fail closed** on anything that is not a definitive not-found.

`internal/statesource/statesource.go` defines a sentinel `ErrNotFound` and a helper `IsNotFound(err)` that also recognizes `fs.ErrNotExist` (filesystem-backed connectors such as local and git surface that; the rest wrap `ErrNotFound` explicitly). Connectors must return `ErrNotFound` *only* when the state is genuinely absent, and a plain error for any transient or unexpected failure.

The edit handler enforces the rule (`EditState`, `internal/api/edit.go`):

```go
cur, readErr := conn.Read(ctx, key)
if readErr != nil && !statesource.IsNotFound(readErr) {
    c.JSON(http.StatusBadGateway, gin.H{"error": "cannot verify current state before writing: " + readErr.Error()})
    return
}
```

A transient read failure aborts the write with `502 Bad Gateway`. Only `readErr == nil` proceeds with the full backup + serial/lineage guard, and only a genuine `IsNotFound` proceeds without a backup (treated as a legitimate first write). The restore path (`RestoreBackup`) applies the same rule, with the same comment: a transient read failure aborts because the current state could not be backed up.

The serial/lineage guard layers on top: when the current state *was* read, a new serial lower than the current serial or a lineage mismatch returns `409 Conflict`, overridable only with `?force=true`.

## Consequences

**Easier**:

- A write can never silently skip its pre-write backup or its anti-regression check because of a backend hiccup — the dangerous case is turned into a clean `502` the operator can retry.
- Every applied edit has a recoverable backup, so the UI can offer one-click revert with confidence.
- The "first write to a new key" case still works without special-casing in each connector — it falls out of the `ErrNotFound` sentinel.

**Harder**:

- Every connector author carries a contract obligation: return `ErrNotFound` for genuine absence and a *distinct* error for everything else. A connector that collapses both into one error (e.g. returning `ErrNotFound` on a timeout) would defeat the guard, so this is exercised by the connector tests.
- A flaky backend produces `502`s on edits rather than best-effort writes; this is intentional but means edit availability is bounded by backend read availability.
- `force=true` exists as an operator escape hatch for the serial/lineage check, and its use is audited — but it does not bypass the fail-closed read check, only the regression comparison.
