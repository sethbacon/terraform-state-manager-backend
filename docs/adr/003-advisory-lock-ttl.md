<!-- markdownlint-disable MD013 -->
# 3. Application-Level Advisory Locks with a 15-Minute Stale TTL

**Status**: Accepted

## Context

Before mutating a state file, the edit pipeline must guarantee mutual exclusion: two concurrent editors of the same `(source, key)` must not clobber each other. Some backends provide native locking that the State Manager uses directly — HCP/TFC workspace lock-then-verify, Consul session locks on `<key>/.lock` (the same key Terraform's consul backend locks), local lock files. But several common backends provide **no native state lock at all**: S3, GCS, Azure Blob, and git. For those, the application must supply its own lock.

An application-level lock raises the classic orphaned-lock problem. Locks are acquired and released within a single HTTP request, with `release()` deferred. But a process can crash, get OOM-killed, or be force-terminated between acquiring the lock and releasing it. Without a backstop, a single crash would **wedge a state key forever** — no future edit could acquire the lock, and the only recovery would be manual database surgery.

The options for bounding orphaned locks were:

1. A heartbeat/lease the editor must renew — correct, but adds a background renewal loop and a renewal-failure story to every edit, which is heavy for request-scoped work.
2. An external lock service (Redis, etcd, ZooKeeper) — adds an operational dependency the deployment does not otherwise need.
3. A TTL-based reap in the same PostgreSQL the app already requires — no new moving parts, atomic at the database.

## Decision

Implement advisory locks as rows in a `state_locks` table guarded by a `UNIQUE(source_id, state_key)` constraint, with a **15-minute stale TTL** that reaps orphaned locks on the next acquire (`internal/db/repositories/lock_repository.go`).

```go
const staleLockTTL = "15 minutes"
```

`Acquire` first deletes any lock on the key older than `staleLockTTL`, then inserts a new lock row. If the key is already held by a live lock, the unique constraint rejects the insert and `Acquire` returns `ErrLocked`, annotated with the holder and acquisition time so an operator can decide whether to force-unlock. The reap + insert is atomic at the database — there is no read-then-write race.

The edit handler chooses the lock implementation per connector (`acquireLock`, `internal/api/edit.go`): if the connector implements the `statesource.Locker` interface it uses the **native** backend lock; otherwise it falls back to this app-level advisory lock. Native-locked backends do not use `state_locks` at all.

15 minutes was chosen as the bound because locks are **request-scoped** — they are held only for the duration of one edit's validate → backup → write → audit pipeline, which is seconds, not minutes. Any lock older than 15 minutes therefore cannot belong to a live request; it is an orphan from a crashed process, and reaping it is safe. The window is generous enough that even an unusually slow backend write cannot have its lock reaped out from under it, while short enough that a crash does not strand a key for long.

For the case where a key is wedged by a crash but the lock has **not yet** aged past the TTL, an admin escape hatch exists: `DELETE /sources/:id/state/lock` (`ForceUnlock`, `admin` scope) calls `ForceRelease`, which removes any lock on the key regardless of holder and audits the action. Force-unlock deliberately touches only the app-level advisory lock, never native backend locks (HCP workspace locks, Consul/local locks) — those are owned by the backend itself.

## Consequences

**Easier**:

- Backends with no native lock (S3, GCS, Azure Blob, git) still get correct mutual exclusion, reusing the PostgreSQL dependency the app already has — no Redis/etcd to operate.
- A crashed editor cannot wedge a key permanently: the next acquire after 15 minutes reaps the orphan automatically, and an admin can force-unlock sooner.
- Acquisition is atomic and race-free; the holder is named in the conflict error so operators have the context to act.

**Harder**:

- The 15-minute bound is a fixed constant, not configurable. A pathologically slow legitimate write that exceeded 15 minutes could in principle have its lock reaped — acceptable given the request-scoped design and HTTP server timeouts, but a real coupling to keep in mind if the edit pipeline ever grows long-running steps.
- Two locking models coexist (native vs. app-level), so reasoning about a given source's locking requires knowing which connector backs it.
- App-level locks protect only writes that go *through this service*. A state file written directly by `terraform apply` against an unlocked backend (S3/GCS/Azure/git) is outside this lock's scope — the serial/lineage guard ([ADR 002](002-fail-closed-state-writes.md)) is the second line of defense against that race.
