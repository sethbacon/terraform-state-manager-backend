# Observability

## Metrics

Prometheus metrics on a **separate, unauthenticated** port (default 9090 —
never expose it publicly): `http_requests_total{method,path,status}`,
`http_request_duration_seconds` (histogram), `db_connections_{open,in_use,idle}`,
`app_info{version}`, plus Go runtime metrics.

- Kubernetes: `serviceMonitor.enabled=true` (+ `prometheusRule.enabled`,
  `grafanaDashboard.enabled`) in the chart; NetworkPolicy already admits the
  `monitoring` namespace on 9090.
- Compose/binary: `deployments/observability/prometheus.yml` scrapes both
  the API replica and the worker.

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

## Audit trail

Application-level audit events (logins, source changes, state edits,
transfers, drift acks, API-key lifecycle) are stored in the database and
surfaced at Administration → Audit logs — they are the compliance record;
Prometheus is capacity/health only.
