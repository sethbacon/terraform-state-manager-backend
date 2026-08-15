# Troubleshooting

## Boot / readiness

| Symptom | Likely cause | Check |
|---|---|---|
| Pod crash-loops at boot | Missing `TSM_JWT_SECRET` (prod refuses to start) | logs: "JWT secret" |
| `/ready` 503, `/health` 200 | DB unreachable / wrong password / sslmode | `TSM_DATABASE_*`; try `psql` with the same DSN |
| "failed to run identity migrations" | DB user lacks CREATE on the database (identity schema) | grant or pre-create schema |
| Healthcheck red but curl 200 | Your probe sends HEAD (`wget --spider`); routes are GET-only | use a GET probe (`wget -O /dev/null` / k8s httpGet) |

## Auth

| Symptom | Cause | Fix |
|---|---|---|
| OIDC redirect loop / cookie not set | `TSM_SERVER_PUBLIC_URL` scheme mismatch (Secure cookie on http) | public URL must match the real scheme/host |
| OIDC "issuer mismatch" | issuer URL differs from the discovery document (trailing slash, http vs https) | copy issuer from `/.well-known/openid-configuration` |
| Group mappings don't apply | claim name mismatch / Entra emits group GUIDs | set `TSM_AUTH_OIDC_GROUP_CLAIM_NAME`; map the GUIDs |
| User lost access after login | reconciliation removed membership when they left a mapped group | expected; re-add to group |
| API key 401 | expired / revoked / wrong prefix; keys are header-only (never cookies) | check `/admin/apikeys` last-used + expiry |

## Drift / Version Lab

| Symptom | Cause | Fix |
|---|---|---|
| Runs stuck "dispatched" | CI never called back: callback URL unreachable from runners | wizard preflight; `TSM_SERVER_CALLBACK_URL` must be public for hosted runners |
| Callback 401/409 | token replay or run already completed | one-shot by design; dispatch a new run |
| Ingest 413 | plan JSON > 5 MiB | send counts/summary instead of the raw plan |
| Schedules never fire | no worker replica running (`TSM_WORKERS_ENABLED`) | exactly one worker must be up — check the worker deployment/service |
| Schedules fire twice | two workers (replicas>1, or workers enabled on API pods) | restore the worker topology |

## State sources

| Symptom | Cause | Fix |
|---|---|---|
| git source fails in containers | `git` binary missing in custom images | official image includes it; keep it |
| HCP 429s during backfill | rate limiting | the syncer retries paced; transient |
| Source shows decrypt errors | `TSM_ENCRYPTION_KEY` changed/lost | re-enter credentials ([disaster-recovery.md](disaster-recovery.md)) |
| local source empty | PVC/mount path mismatch with the source's base_path | `localStates.mountPath` vs source config |
| local source rejected: "base_path is outside the permitted local state-source roots" / "no permitted base_path roots are configured" | `TSM_STATESOURCE_LOCAL_ROOTS` doesn't list the directory (it is empty by default and permits nothing) | add the mounted directory to `TSM_STATESOURCE_LOCAL_ROOTS` and restart ([configuration.md](configuration.md#state-sources)) |
| kubernetes source rejected: "kubeconfig path is outside the permitted roots" | same boundary for `config.kubeconfig` | list its directory in `TSM_STATESOURCE_KUBECONFIG_ROOTS`, or configure `config.server` + `credentials.token` instead |

## Suite / shared identity

| Symptom | Cause | Fix |
|---|---|---|
| Shared-schema role scopes reset on every restart | two apps share one identity DB but both seed it (`role_seed_owner=self` on both) | set `TSM_SUITE_ROLE_SEED_OWNER` so exactly one app owns that seed (`registry` or `tsm`). TSM's **own** role scopes are unaffected — it seeds and reads its own copy |
| A user lost access, or kept access they should not have, after an upgrade | TSM's role tables and the shared identity schema disagree | run `tsm-server authz-drift`. Non-zero names the pairs. Restart the backend (the startup reconcile repairs and now logs what it changed) and re-run; drift that **survives a restart** means the reconcile is failing, and the startup log says why. Roll back with `TSM_AUTHZ_ROLE_SOURCE=identity` |
| Every role read fails with "no role source was configured" | `TSM_AUTHZ_ROLE_SOURCE` is set to something that is neither `app` nor `identity` | the boot refuses an unknown value outright; if the process started, check the startup line `authorization role source` for what it actually resolved |
| Roles a coupled deployment relied on changed meaning after upgrading | TSM now authorizes from its own role definitions, not the sibling's (the intent of the per-app model) | expected. `tsm-server authz-drift` on the pre-upgrade build names the affected roles via `template_drift`; `TSM_AUTHZ_ROLE_SOURCE=identity` restores the previous behaviour |
| "Consumed by" panel empty / `GET /consumers` 401 | service tokens don't match (or `TSM_SUITE_SERVICE_TOKEN` empty ⇒ endpoint disabled) | set `TSM_SUITE_SERVICE_TOKEN` to the SAME value as the sibling registry's `TFR_SUITE_SIBLING_TOKEN` |
| Module freshness / cross-app features missing | sibling not discovered | set `TSM_SUITE_SIBLING_URL`; check `TSM_SUITE_POLL_INTERVAL` and that the sibling manifest is reachable |

## Production vs dev differences that bite

- No Keycloak in production — the dev realm (admin.user etc.) is a dev-stack
  artifact; production uses your IdP.
- `DEV_MODE` off ⇒ no `/api/v1/dev/login`; e2e cookie flows need a dev stack.
- `TSM_LOGGING_LEVEL=warn` hides info-level boot lines — drop to `info` when
  debugging startup.

Still stuck: grep the worker logs for `statesync`/scheduler lines, check
Administration → Audit logs for the failing operation, and compare your env
against [configuration.md](configuration.md).
