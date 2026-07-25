<!-- markdownlint-disable MD013 MD060 -->
# Capacity Planning

This guide provides sizing recommendations for the Terraform State Manager (TSM)
backend across deployment scales. Unlike a registry, TSM does not warehouse state
as a general-purpose artifact store — it analyzes and watches state **where it
already lives**, copying it into its own database only as analysis metadata and
as pre-write backups (full copies of the state file, kept so an edit is
one-click reversible — see [Database sizing](#database-sizing)). Its scaling
axis is the **number of state files** under management and the **cadence** at
which they are synced, not general artifact storage. The database holds
analysis metadata, pre-write state backups, drift/health runs, and the shared
identity tables.

## Table of Contents

1. [Sizing axis: state files](#sizing-axis-state-files)
2. [Database sizing](#database-sizing)
3. [Sync cadence & worker load](#sync-cadence--worker-load)
4. [Connection pool sizing](#connection-pool-sizing)
5. [Compute recommendations](#compute-recommendations)
6. [Network bandwidth](#network-bandwidth)

---

## Sizing axis: state files

The primary driver is the count of state files across all configured sources. A
useful reference frame:

| Scale  | State files     | Sources | Typical setting   |
| ------ | --------------- | ------- | ----------------- |
| Small  | < 100           | 1–5     | A team or two     |
| Medium | 100 – 1,000     | 5–25    | A division        |
| Large  | 1,000 – 10,000+ | 25+     | An org-wide fleet |

A background syncer maintains a **persistent analysis store** so dashboards and
reports stay fast at thousands of state files — the heavy work (fetching and
analyzing state) happens on a schedule, not on the request path.

---

## Database sizing

TSM stores **analysis metadata plus pre-write state backups** — it is not a
metadata-only footprint. The **live/current** state is always fetched on demand
from the source and is never persisted as the canonical copy, but every edit,
`rm`/`mv`, or restore takes a **pre-write backup that stores a full copy of the
raw state bytes** (`state_backups.data`, a `BYTEA NOT NULL` column — see
[architecture.md § State Edit Pipeline](architecture.md#state-edit-pipeline) and
[disaster-recovery.md](disaster-recovery.md)), not just its metadata. Transfer
is the exception: it overwrites the target with no backup of the target's
prior content, and only writes a backup row — of the source — when a migrate
decommissions its source and verification succeeds. The fastest-growing
tables are the per-state analysis history, the audit log (in the shared
`identity` schema), drift run history, and — for fleets with a high
edit/restore/transfer rate or large state files — `state_backups` itself,
since each row holds a full state-file copy rather than a small metadata row.

### Row estimates by scale

| Table                                                                  | Small (<100 states) | Medium (100–1,000) | Large (1,000+)   |
| ---------------------------------------------------------------------- | ------------------- | ------------------ | ---------------- |
| `state_sources`                                                        | ~5                  | ~25                | ~100             |
| `state_analyses` (one live row per state)                              | ~100                | ~1,000             | ~10,000          |
| `state_analysis_history` (append-only)                                 | ~1,000              | ~50,000            | ~1,000,000+      |
| `state_module_refs`                                                    | ~500                | ~10,000            | ~100,000         |
| `drift_runs`                                                           | ~1,000/month        | ~20,000/month      | ~200,000/month   |
| `drift_records` (one live per state)                                   | ~100                | ~1,000             | ~10,000          |
| `health_runs`                                                          | ~500/month          | ~10,000/month      | ~100,000/month   |
| `state_backups` (full state copies, one row per edit/restore/decommissioning transfer) | ~100                | ~2,000             | ~20,000          |
| `audit_logs` (identity schema)                                         | ~10,000/month       | ~100,000/month     | ~1,000,000/month |

> **`state_backups` doesn't follow the same sizing rule as the other rows
> above.** Every other table's rows are small, fixed-shape metadata records, so
> row count alone predicts size. `state_backups` rows instead hold a **full
> copy of the state file being backed up**, so its footprint scales with
> `row_count × avg_state_size`, not row count alone — see
> [Disk sizing](#disk-sizing).

### What drives growth

- **`state_analysis_history` is append-only, but bounded by change.** A new
  history row is recorded **only when the observed state content actually
  changes** — a sync cycle that finds a state unchanged writes nothing. Growth
  therefore tracks how often your states change, not the sync interval. Fleets
  with frequent applies grow this table fastest.
- **`state_backups` grows with every edit, `rm`/`mv`, or restore, plus a
  decommissioning migrate — and does not self-prune.** Transfer is the only
  operation that doesn't always add a row: a plain backup, and a migrate
  without `decommission` (or one whose verification fails), write zero
  `state_backups` rows, since the target's prior content is never backed up.
  Unlike `state_analysis_history` (auto-pruned after 180 days by the sync
  worker's `PruneHistory`), `state_backups` has **no automatic retention or
  pruning** today. The only way to remove backups is an admin purge
  (`purge=true` on delete), which also removes the state object itself —
  there is no "prune old backups, keep the state" operation. Plan for
  unbounded growth (or a manual pruning process) until the planned retention
  policy ships (tracked in #257).
- **`audit_logs`** (logins, source changes, state edits, transfers, drift acks,
  API-key lifecycle) lives in the **shared `identity` schema**. In a coupled
  suite with a federated registry, it also receives the registry's federated
  entries — size for the combined volume. See
  [suite-coupling.md](suite-coupling.md).
- **Drift and health runs** accumulate per scheduled or CI-pushed run; one row
  per run plus per-resource findings.

### Disk sizing

TSM's footprint is mostly metadata-shaped, but **not entirely**: `state_backups`
holds full state-file copies, so its contribution scales with
edit/restore/transfer volume and state-file size, not row count alone. The
estimates below assume typical state files (tens of KB to a few MB) and a
moderate edit rate; fleets with very large state files or a high
edit/restore/transfer rate should add `backup_count × avg_state_size` on top.

| Scale  | Estimated DB size | Recommended disk |
| ------ | ----------------- | ---------------- |
| Small  | 100 – 500 MB      | 5 GB             |
| Medium | 500 MB – 3 GB     | 20 GB            |
| Large  | 3 – 20 GB         | 50–100 GB        |

Most of the long-tail growth is `state_analysis_history`, `drift_runs`,
`audit_logs`, and `state_backups`. `state_analysis_history` is auto-pruned
after 180 days by the sync worker; `drift_runs` and `audit_logs` are not pruned
by the application, so plan periodic pruning of old run/history rows per your
retention policy. **`state_backups` is not pruned by the application either**,
and unlike the others there is no way to prune it while keeping the state
itself — only a full state-object purge (`purge=true` on delete) removes its
backups. Plan capacity for unbounded backup growth until the planned retention
policy ships (tracked in #257).

`state_backups` is also stored **unencrypted** at rest today, protected only by
database access control and the `state:read`-scoped API — unlike source/CI
credentials and other secrets, which are AES-256-GCM encrypted (see
[threat-model.md § Assets](threat-model.md#4-assets) and
[../SECURITY.md](../SECURITY.md)). Encryption at rest is planned alongside the
retention policy (tracked in #257).

---

## Sync cadence & worker load

The background **state-sync reconciler** keeps the analysis store current. Its
behaviour and tuning frame your sizing:

- **Interval:** the reconcile loop runs every **5 minutes**, and once
  immediately on boot so a fresh start backfills the store.
- **Per-source budget:** each source gets up to **10 minutes** per cycle
  (generous on purpose — a first backfill of a large org may take minutes, and
  nothing waits on it).
- **Per-source read concurrency: 6**, kept modest deliberately. HCP Terraform
  rate-limits around 30 requests / 30 s per token, and every state read there is
  two round-trips — so a high concurrency would trip the limit, not go faster.
- **Changed-only reads:** the syncer lists states, then reads only those whose
  observed content changed, and records history only on a real change.
- **On-demand syncs** (post-edit refresh, source-create backfill) run on **every
  replica** regardless of the worker gate, and a `?refresh=true` request serves
  the current store rather than blocking if a cycle is already running (only one
  cycle runs at a time).

### Worker topology — this affects sizing

The periodic loops (the 5-minute state-sync reconciler **and** the 60-second
schedule runner) must run on **exactly one replica**. The schedule runner has no
cross-replica claim, so two replicas would double-dispatch due schedules.
Therefore:

- API replicas run with `TSM_WORKERS_ENABLED=false` and can scale/HPA freely.
- **Exactly one** dedicated worker replica runs with `TSM_WORKERS_ENABLED=true`
  (use a `Recreate` rollout so two workers never overlap).

Size the **worker** for the sync load (it does the fetch+analyze work); size the
**API replicas** for request concurrency. See
[deployment/README.md](deployment/README.md#worker-topology).

### Estimating a full reconcile

```text
cycle_time_per_source ≈ (changed_states / 6) * avg_read_seconds
```

With thousands of states spread across many sources, keep states-per-source
balanced so no single source exceeds its 10-minute budget. If a source routinely
hits the budget, split it into multiple sources or accept that its backfill spans
several cycles (nothing breaks — the store is eventually consistent).

---

## Connection pool sizing

`TSM_DATABASE_MAX_CONNECTIONS` (default **25**) is a cap **per replica**, so your
server must support `replicas × max_connections`. There are two pools when the
identity store is separate (`TSM_IDENTITY_DATABASE_*`) — size both. Tune with:

```text
recommended_pool = (api_replicas * 2) + worker_jobs + headroom
```

Where:

- `api_replicas * 2` — each API pod uses ~2 connections for request handling.
- `worker_jobs` — the dedicated worker's sync + schedule loops (~6 connections,
  matching its read concurrency).
- `headroom` — 5–10 connections for spikes.

**Example:** 3 API replicas + 1 worker → `(3 × 2) + 6 + 6 = 18`. The default 25
leaves comfortable headroom. For large deployments (10+ replicas) put PgBouncer
in front of PostgreSQL and reduce per-replica `max_connections` accordingly.

> `min_idle_connections` (default 5) keeps warm connections ready; leave it at
> the default unless you see cold-start latency on the first request after idle.

---

## Compute recommendations

### API replicas

| Scale  | CPU request | CPU limit | Mem request | Mem limit | Replicas |
| ------ | ----------- | --------- | ----------- | --------- | -------- |
| Small  | 100m        | 500m      | 128 Mi      | 512 Mi    | 2        |
| Medium | 250m        | 1000m     | 256 Mi      | 1 Gi      | 2–3      |
| Large  | 500m        | 2000m     | 512 Mi      | 2 Gi      | 3–6      |

API requests are lightweight (metadata reads from the analysis store). The
heaviest API-path operations are ad-hoc analysis of an uploaded state
(`POST /analyze`) and report generation, which are CPU/memory-bound on the size
of the state file.

### Worker replica (dedicated, always 1)

| Scale  | CPU request | CPU limit | Mem request | Mem limit |
| ------ | ----------- | --------- | ----------- | --------- |
| Small  | 100m        | 500m      | 128 Mi      | 512 Mi    |
| Medium | 250m        | 1000m     | 256 Mi      | 1 Gi      |
| Large  | 500m        | 2000m     | 512 Mi      | 2 Gi      |

The worker does the fetch+analyze work; analyzing large state files is the main
memory driver. Give it more memory headroom than an API pod at the large scale.

### PostgreSQL

| Scale  | CPU     | Memory | Storage IOPS |
| ------ | ------- | ------ | ------------ |
| Small  | 1 vCPU  | 2 GB   | 100          |
| Medium | 2 vCPU  | 4 GB   | 500          |
| Large  | 4+ vCPU | 8+ GB  | 1000+        |

Use a managed PostgreSQL (Azure Database for PostgreSQL, RDS, Cloud SQL) for
production; enable connection pooling for large deployments. PostgreSQL 14+ (16
recommended).

### HorizontalPodAutoscaler

Enable the HPA on **API replicas only** — never let the HPA scale the dedicated
worker (it must stay at one replica):

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: 80
```

---

## Network bandwidth

TSM's network use is dominated by **fetching state from sources** during sync,
plus outbound calls to CI and (when coupled) the sibling registry.

### State-fetch bandwidth (worker → sources)

```text
sync_bandwidth ≈ changed_states * avg_state_size / sync_window
```

State files are typically small to moderate (tens of KB to a few MB), and only
changed states are read each cycle — so sustained bandwidth is modest even at the
large scale. Co-locate TSM with the state backends (same region/VPC) to minimize
fetch latency, especially for S3/Blob/GCS sources.

### CI and cross-app traffic

- **Drift / Version Lab callbacks:** CI runners POST results to
  `TSM_SERVER_CALLBACK_URL`, which must be reachable from your runners. Payloads
  are JSON (plan summaries / findings), not large.
- **Suite coupling (when enabled):** the manifest poll (every 60 s, 2 s timeout),
  the registry's `/consumers` proxy, the freshness reverse call, and audit
  federation are all small JSON exchanges. See
  [suite-coupling.md](suite-coupling.md).

### Recommendations

| Scale  | Minimum bandwidth | Recommended |
| ------ | ----------------- | ----------- |
| Small  | 100 Mbps          | 100 Mbps    |
| Medium | 100 Mbps          | 1 Gbps      |
| Large  | 1 Gbps            | 1 Gbps      |

For large fleets, the dominant factor is fetch latency to the state backends, not
raw bandwidth — placing the worker close to the sources matters more than a fat
pipe.
