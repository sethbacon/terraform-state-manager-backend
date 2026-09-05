# Observability

## Metrics

Prometheus metrics on a **separate, unauthenticated** port (default 9090 —
never expose it publicly): `http_requests_total{method,path,status}`,
`http_request_duration_seconds` (histogram), `db_connections_{open,in_use,idle}`,
`app_info{version,go_version,build_date}`, plus Go runtime metrics.

Worker & sync health (worker-enabled replica only):
`tsm_worker_last_tick_timestamp_seconds{worker}` (statesync / scheduler /
driftreconcile / healthreconcile loop heartbeats — alert on
`time() - metric > 3 * interval` to catch a wedged loop that `TSMTargetDown`
cannot see, since the process and metrics port stay up),
`tsm_source_last_sync_timestamp_seconds{source_id,source_name}` and
`tsm_source_sync_read_errors{source_id,source_name}` (per-source sync
freshness and last-cycle read errors). `/ready` on the worker replica also
fails when any registered loop stops ticking past its staleness budget
(3 intervals, 2-minute floor), so Kubernetes surfaces a stalled worker
without any Prometheus rule.

Fleet-scale scheduler pacing (`TSM_DRIFT_MAX_IN_FLIGHT`,
`TSM_SCHEDULER_BATCH_LIMIT` — see [configuration.md](configuration.md#workers)):
`tsm_drift_runs_in_flight` (gauge — the scheduler's most recent
sample of drift runs currently `dispatched`/`running`, the same count its
in-flight cap checks before claiming each due schedule), `tsm_scheduler_due_backlog`
(gauge — due schedules the most recent poll read but did not claim, whether
deferred under the cap or a failed claim attempt; a schedule claimed by a
concurrent replica does not count towards it), and `tsm_drift_dispatch_total{result}`
(counter — every schedule firing's terminal result: `ok`, `failed`, or
`deferred`). A sustained non-zero `tsm_scheduler_due_backlog`, or a rising rate
of `tsm_drift_dispatch_total{result="deferred"}`, means due work is arriving
faster than the configured cap can drain it — raise `TSM_DRIFT_MAX_IN_FLIGHT`
or the shared agent pool's capacity.

- Kubernetes: `serviceMonitor.enabled=true` (+ `prometheusRule.enabled`,
  `grafanaDashboard.enabled`) in the chart; NetworkPolicy already admits the
  `monitoring` namespace on 9090.
- Compose/binary: `deployments/observability/prometheus.yml` scrapes both
  the API replica and the worker.
- `deployments/observability/recording-rules.yml` precomputes the common
  series the dashboard/alerts use — request rate, 5xx ratio, p99 latency, and
  DB-pool utilization — load it alongside the scrape config.

## Alerts

`deployments/observability/alert-rules.yml` / the chart's PrometheusRule:
`TSMTargetDown` (critical — if it's the **worker**, schedules and state sync
have silently stopped), `TSMHigh5xxRate` (>5% 10m), `TSMHighLatencyP99`
(>5s 10m), `TSMDBPoolSaturated` (>90% 10m).

## Dashboard

`deployments/observability/grafana-dashboard.json` (or the chart's ConfigMap
for sidecar provisioning): request rate by status, p50/p95/p99 latency, 5xx
ratio, DB pool, build info.

## Logs

Structured slog. `TSM_LOGGING_FORMAT=json` in production;
level `warn` keeps volume low but **suppresses boot/info lines** (including
the worker-gate notice) — drop to `info` when diagnosing. Useful components:
`statesync` (per-source sync results), scheduler dispatches,
`statesource.hcp`/`drift` mutation logs, auth provider initialization.

Every request emits one `http_request` access-log line (info level, so hidden
at `warn`) with `method`, `path` (route template), `status`, `latency_ms`,
`client_ip`, `user_id` when authenticated, and `request_id` — the same value
echoed in the `X-Request-ID` response header, so a client-reported ID can be
correlated with server logs. Probe paths (`/health`, `/ready`, `/metrics`)
are excluded.

No profiling endpoint (`pprof`) is exposed — there is no `TSM_TELEMETRY_PROFILING_*`
setting; the only side-channel port is `/metrics`.

## Audit trail

Application-level audit events (logins, source changes, state edits,
transfers, drift acks, API-key lifecycle) are stored in the database and
surfaced at Administration → Audit logs — they are the compliance record;
Prometheus is capacity/health only.
