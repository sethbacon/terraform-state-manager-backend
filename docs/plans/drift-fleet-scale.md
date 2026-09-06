# Plan: TSM as the Fleet Drift Dashboard + Scheduler (repo-level fan-out)

> **Status:** **Partially implemented 2026-09-05.** Phases 1, 1b, 2 and 4a landed in
> sethbacon/terraform-state-manager-backend#569; Phase 4b in sethbacon/terraform-state-manager-frontend#416.
> Phase 0 (ops), Phase 3 (tooling) and Phase 5 (contract) are **not started**.
> See "Implementation status" below before working from this document.
> **Repo:** `terraform-state-manager-backend` (primary) + `terraform-state-manager-frontend`,
> the Brunswick drift templates (operator data in the TSM template registry), the Brunswick
> onboarding tooling, and — Phase 5 only — `terraform-drift-contract`, `driftingest`,
> `azure-pipelines-terraform`, `terraform-drift-report`.
> **Scope:** Make TSM the central drift scheduler and dashboard for an org of ~300
> repositories × ~5 states on a **private** TSM, **private** registry and **self-hosted**
> Azure DevOps scale sets — by changing the unit of work from *app* to *repo*, pacing the
> scheduler, onboarding by discovery, and building the dashboard read-path. Push/ingest is
> kept as an escape hatch, not the default.
> **Audience:** implementing agents with less context. Every task names the files, the
> shapes, the tests and a "done when". Do not redesign; if a seam listed here has moved,
> stop and report.

---

## 0. Implementation status (2026-09-05)

| Phase | Status | Where |
| --- | --- | --- |
| 0 — Environment prerequisites | **verified 2026-09-06 — partially met**: CA-in-image and callback base already OK; drift pool, ADO identity and the 3.21 deployment outstanding (see Phase 0) | ops only, no code |
| 1 — Repo-level fan-out dispatch | **done (backend)** | sethbacon/terraform-state-manager-backend#569 |
| 1.4 — Brunswick operator templates | **not done** | templates live outside this repo |
| 1b — Workload Identity | **done, except item 3** — per-target `Params` (per-app service connection) is not implemented and gates 1.4 | sethbacon/terraform-state-manager-backend#569 |
| 2 — Scheduler pacing | **done** | sethbacon/terraform-state-manager-backend#569 |
| 3 — Discovery-driven onboarding | **not started** | tooling host unreachable |
| 4a — Dashboard read-path (backend) | **done** | sethbacon/terraform-state-manager-backend#569 |
| 4b — Dashboard read-path (frontend) | **done** | sethbacon/terraform-state-manager-frontend#416 |
| 5 — Contract: infra drift vs unapplied | **not started** | sequenced last by design |

### Corrections — this document's §3 anchors were stale within a day

Recorded because §3 presents itself as verified current state, and an implementer
following it literally would have introduced a security regression.

1. **The tenancy/authorization layer is missing from §3 entirely.** `dispatchDrift` is a
   method on `*DriftHandlers` carrying an `auth dispatchAuthority` parameter, and
   `driftDispatcher.Dispatch` carries `derived tenancy.SystemScope` — neither appears in
   this plan. `dispatch_ownership.go` and `callback_root_scope.go` did not exist when §3
   was written. **Following §5 Phase 1's stated signatures verbatim would have dropped
   those parameters**, silently removing the InScope reads that keep a dispatch inside one
   organization. The implementation preserved them and extended the source check into a
   per-item loop, so every fan-out target's state source is now scope-verified.
2. **Migration numbers.** §5 says `000033`/`000034`/`000035`. Those were taken
   (`organization_partition`, `…_phase4`, `legal_holds`, `group_mappings`). Phase 1 used
   **`000037`**, Phase 1b **`000038`**. Phase 4a needed no migration at all — the indexes
   from `000037` and `000029` already serve its queries.
3. **The coverage floor is 80, not 79.** `.github/workflows/ci.yml` sets `THRESHOLD=80`, a
   documented ratchet raised on 2026-07-16. Filtered coverage after this work is 80.7 %,
   so the margin is thin.
4. **Line numbers throughout §3 are stale** — locate seams by name.
5. **The two adjacent auth plans were already flipped to `Implemented`** in `3865d03`, the
   same commit that added this document; §5 Phase 1b's housekeeping item was already done.
6. **`GET /drift/summary` has no fleet-wide `stale` field**, so §5 Phase 4b's landing-card
   list could not be built as written. Staleness is per-source on `/drift/coverage`, and
   computing it fleet-wide would defeat that endpoint's 60 s cache. `in_flight` was
   substituted.

### Deviations worth knowing

- **Retention** is `AttachDriftRecords`/`EnableRetention` methods on the reconciler,
  mirroring `Syncer.EnableBackupRetention`, rather than the `Pruner` interface §5 Phase 4a
  suggests. Same behaviour, existing idiom, no new goroutine.
- **`createDriftRun` returns `DriftRun | DriftBatch`** — a single run for ≤1 target, a
  batch for 2+. §5 did not spell the response type out.
- **Coverage staleness is anchored on `created_at`** (dispatch time), matching the TTL
  expiry semantics used elsewhere, not `updated_at`.

### Verification gaps — read before rollout

- **Spike 1.0 has now been run** (2026-09-05, live `bconline` org — see "Spike results").
  The `type: object` coercion **holds**; the token-exposure result changes the
  recommendation for the fan-out template (secret run variables, not template
  parameters). The "string fallback" is **not** an exposure mitigation.
- **No live ADO or GitHub dispatch** was exercised anywhere; CI paths are covered by fake
  HTTP servers. Workload Identity was verified against the handler's accepted request
  shape, not a real federated-credential exchange.
- Phase 4a's four organization-scoped readers **are** proven against real PostgreSQL: each
  predicate was replaced with a tautology and each test failed by returning the other
  organization's row. sqlmock alone could not have shown this — it matches query text and
  never evaluates a predicate.

---

## 1. Motivation

TSM already has every primitive an org-wide drift system needs: dispatch → one-shot-token
callback, durable acknowledgeable `drift_records`, a correct at-most-once cron scheduler, a
reconciler for stuck runs, an operator template registry, notifications, and a push
`/drift/ingest` endpoint. What it lacks is **shape for a fleet**:

- Every unit of work is **one app** (one `working_dir`, one token, one run, one ADO job).
  At ~300 repos × ~5 apps that is ~1,500 jobs a night, each paying VM handoff + installer +
  provider mirror + clone on the shared `ubuntu-minimal-scale-set`.
- Registration is **one row at a time** through a wizard that creates no schedule and cannot
  select the Brunswick template profiles.
- The scheduler has **no pacing** (no per-tick limit, no in-flight cap): a nightly cohort
  lands on the agent pool as one herd.
- The UI is a **run viewer, not a dashboard**: completeness markers (stored by the backend)
  are rendered nowhere; there is no coverage/"last checked" view, no CI-run link, no
  per-source aggregates, no retention, no drift metrics.
- The contract counts only `resource_changes`, so "drift" blends hand-edits with unapplied
  commits.

The earlier alternative ("invert to push") is **rejected for this environment**: the private
network makes callbacks trivial, only TSM can pace a finite shared pool, and push writes no
run row so it cannot answer "when was this state last checked?".

**Outcome:** one ADO run per **repo** plans N apps sequentially and reports N results;
~300 schedules instead of ~1,500; the scheduler drains a cohort under a configurable
in-flight cap; TSM shows coverage, freshness, completeness and links to the CI run;
onboarding is a script driven by ADO/blob discovery.

Capacity, illustrative (`agent-min ≈ jobs × (overhead + apps × plan)`, 2 min overhead,
1 min/plan): per-app jobs 1,500 × 3 = **4,500** agent-min (~3.75 h on 20 agents);
per-repo fan-out 300 × (2 + 5) = **2,100** agent-min (~1.75 h) and 5× fewer queue entries.
*Verified 2026-09-06 (Phase 0): the shared Linux pool's real ceiling is **4 agents**, which
turns those into ~19 h and ~8.75 h — a dedicated drift pool is a prerequisite, not an
optimisation.*

## 2. Goals / Non-goals

### Goals

- One dispatch → N runs (one per state) → one ADO job, each run keeping its own one-shot
  token, TTL and record semantics.
- Absolute wire back-compat for existing 3-parameter templates and existing schedules.
- Scheduler backpressure (`max in-flight`) and per-tick bounding.
- Discovery-driven bulk onboarding for the Brunswick fleet (script).
- Dashboard read-path: coverage, freshness, completeness badges, CI-run link, aggregates,
  retention, metrics.
- Contract: separate *infra drift* (`resource_drift`) from *unapplied changes*
  (`resource_changes`), additively.

### Non-goals

- Removing or changing `/drift/ingest` or the callback handler.
- A bulk-registration UI (the script covers it; the UI gets a per-schedule targets editor).
- Per-org/team authorization scoping of on-demand re-checks.
- An ingest mode in the ADO task.
- Batch-level notification digests (N notifications per batch is intentional for now).

## 3. Current-state anchors (verified 2026-09-04)

Backend (`backend/` relative):

- `DriftTarget` `internal/api/drift.go:260-266`; `CreateRun` `:213-256`;
  `dispatchDrift(ctx, tgt, actor) (*repositories.DriftRun, error)` `:275-328` — creates the
  run row **before** dispatch; callback URL =
  `cfg.Server.CallbackBase()+"/api/v1/drift/runs/"+id+"/results"`; on failure
  `UpdateStatus(id,"failed",err)`.
- `pipelines.DispatchAzureDevOps(ctx, cred ADOToken, cfg AzureDevOpsConfig, ref string, params map[string]string) error`
  `internal/pipelines/azuredevops.go:35-87` — sends `{"templateParameters": params, "resources": {...}}`
  and **decodes no response body**. Wrapper `DispatchAzureDevOpsDrift` `:90-96` hardcodes the
  three keys. **Shared with Version Lab** (`internal/api/health.go:137`) — its signature must
  not change. ADO rejects undeclared template parameters ("Unexpected parameter" hint exists).
- `driftDispatcher.Dispatch(ctx, targetType, targetConfig, actor) (runID, status string, err error)`
  `internal/api/schedules.go:23-44`; `scheduleRequest.validate()` `:73-97`; `RunSchedule` `:236`.
- Scheduler `internal/services/scheduler/scheduler.go`: `defaultInterval=60s` (:21),
  `fireTimeout=30s` (:25), `checkDue` (:85-96) serial loop over `GetDue`, `fire` (:104-141)
  claims via `ClaimDue` **before** dispatch (at-most-once, no starvation). `GetDue` SQL has
  **no LIMIT** (`internal/db/repositories/schedule_repository.go:141-144`).
- Callback `RunResults` `drift.go:430-493`; `driftRunResultPayload` `:397-415`;
  `recordDriftOutcome` `drift_records.go:48-83` early-returns unless
  `status=="completed" && SourceID!=nil && StateKey!=""`; unparseable never auto-resolves.
- `DriftRun` + `driftColumns` `internal/db/repositories/drift_repository.go:12-51`;
  `List(ctx, limit, offset, status)` :119; `CountRuns(ctx, status)` :154;
  `ListExpiredDispatched`/`ExpireDispatched` :222/:253.
- Reconciler `internal/services/driftreconcile/reconciler.go`; config
  `DriftConfig{RunTTL, ReconcileInterval}` `internal/config/config.go:96-99`, defaults
  `:700-701` (`TSM_DRIFT_RUN_TTL`, `TSM_DRIFT_RECONCILE_INTERVAL`). Viper `AutomaticEnv`
  with prefix `TSM` and `.`→`_` (`:535-537`).
- Workers `internal/api/workers.go:119-177` (`startWorkers`: scheduler, syncer,
  driftreconcile, healthreconcile). Retention model to copy: `BackupRetentionConfig`
  `config.go:228-245` + `StateEditRepository.PruneBackups(ctx, keep, maxAge)`
  `state_edit_repository.go:128` (window `row_number() OVER (PARTITION BY source_id, state_key …)`).
- Records: `DriftRecordRepository.List(ctx, statuses, sourceID, severity, limit, offset, start, end)`
  :238; `CountsByStatus` :326 (global only); handler response `{records, counts, total}`
  `drift_records.go:346-362`.
- States: `ListStates` `internal/api/sources.go:437-462` →
  `[]statesource.StateRef{Key, Name, Size, LastModified, Version}`.
- Metrics `internal/telemetry/metrics.go:16-22` (promauto GaugeVec example);
  `RegisterWorker/WorkerTick` `worker.go:67/84`. Only `tsm_worker_last_tick_timestamp_seconds`
  exists for drift/scheduler.
- Audit `h.audit.write(c, action, resourceType, resourceID, metadata)` `internal/api/audit.go:15-31`.
- Migrations: latest **000032**; next **000033**. Runner is golang-migrate v4
  (`internal/db/db.go:123`) — one exec per file.
- Tests: sqlmock + `httptest`; drift HTTP rig `newDriftEnv(t)`
  `internal/api/drift_health_http_test.go:18`; dispatch wire tests
  `internal/pipelines/dispatch_test.go`; scheduler `scheduler_test.go`. Coverage floor 79 %
  filtered. Swagger annotations required on every handler; regenerate with `make swag`
  (swag v1.16.4); CI fails on a stale `backend/docs/swagger.json`.

Frontend (`terraform-state-manager-frontend/frontend/`):

- Types `src/services/api.ts`: `DriftRun` :757-774, `DriftRecord` :831-852,
  `ScheduleTargetConfig` :870-876, `Schedule` :878-891, `PipelineConnection` :666-673,
  `DashboardOverview` :244-259. Methods `listDriftRuns` :1499-1511 (`{runs,total}`),
  `createDriftRun` :1512, `listDriftRecords` :1514-1529, schedules :1536-1546,
  `listStates` :1222, `listPipelines/createPipeline` :1425-1428. No `getDriftRun` exists.
- `queryKeys.drift` `src/services/queryKeys.ts:70-78`; `queryKeys.schedules` :83-86.
- Components: `src/pages/DriftPage.tsx` (header actions :71-92),
  `src/pages/drift/DriftRunsTable.tsx` (`+ / ~ / -` :83, :94-96),
  `src/pages/drift/DriftRunDetailDialog.tsx` (counts line :49-57, `SUMMARY_RENDER_CAP=200`),
  `src/components/DriftRecordsSection.tsx` (chips row :399-411; bulk ack/resolve exists),
  `src/pages/SchedulesPage.tsx` (`ScheduleFormDialog` :225-236; `workingDir` :357-363;
  `stateKey` :382-392), `src/components/DriftRepoWizard.tsx` (hardcodes profile `default`).
- Dashboard cards pattern `src/pages/LandingPage.tsx:144-163` (`DashboardCard` from
  `@4cloudguru/cloud-suite-ui`).
- i18n `src/locales/en/translation.json` (`pages.drift.*`, `pages.schedules.*`); other
  locales are DeepL-filled by `.github/workflows/translate.yml` — **edit only `en`**.
- Tests vitest; mock pattern `vi.mock('../services/api', …)` as in
  `src/components/DriftRecordsSection.test.tsx:9-19`; thresholds statements 85 / branches 80 /
  functions 82 / lines 87; `eslint . --max-warnings 0`; `npx tsc --noEmit`.

Templates, tooling, contract:

- Brunswick operator templates (`C:\dev\ado\.drift-assess\templates\`):
  `azure-pipelines-tsm-drift.brunswick-azure-ext.yml` (params `callback_url`,
  `callback_token`, `working_dir` = APP id, `dry_run`, `azure_service_connection`,
  `azure_subscription_id`, `backend_storage_account`, `backend_container`; resolver +
  assemble :85-151; module downloads :209-245; installer :249; init :255; plan :270;
  show-json :296-317; `PipelineTerraformDriftReport@1` :326-334; `condition: failed()`
  fallback :342-369), `…TBD4826-ext.yml`, `…brunswick-oci-ext.yml`.
  `upload-templates-to-tsm.ps1` upserts them into `/api/v1/admin/ci/templates`
  (profiles `brunswick-azure-ext`, `brunswick-azure-sql-ext`, `brunswick-oci-ext`).
- Built-in ADO template consts `internal/api/drift_workflows.go` (`azureDriftPipeline` :186,
  `azureDriftPipelineSuite` :412); seeding via `builtinWorkflowSeeds`.
- Brunswick repo-scan helpers `C:\dev\ado\.drift-assess\select_repos.py` (`api()`,
  `has_tf_in_resource_inputs`), `scan_repos.py`, and
  `oci_decompose.parse_active_tops()` in `terraform-migration-tools` (active stages from
  `pipeline.yml`). TSM auth for scripts: `.tsmbase`/`.tsmtoken` (0600) or `TSM_API_KEY`.
- ADO task `PipelineTerraformDriftReport@1` v1.23.0
  (`azure-pipelines-terraform/Tasks/TerraformDriftReport/TerraformDriftReportV1`): body =
  `{...summarize(plan), status:'completed', detail}` (`src/index.ts:203-207`); callback-token
  only; depends on `@4cloudguru/terraform-drift-contract ^1.3.0`. Release = release-please +
  **Minor bump of every touched task** (`scripts/bump-minor-versions.js`, gate
  `Release PR Minor Bumps`).
- Contract `terraform-drift-contract` v1.2.0: `Result{added, changed, destroyed, drifted, summary, unparseable, unmasked, truncated, omitted_entries, omitted_attrs}`
  (`src/summarize.ts:104-139`); `Plan{resource_changes?, configuration?}` :60-73; corpus
  `conformance/vectors.json` (56 vectors) with `CORPUS_SHA256`/`RECONCILED_DIGEST`; Go mirror
  `backend/internal/services/driftingest/plan.go` (`Result` :108-132, vendored corpus
  `testdata/conformance/vectors.json`, digests `conformance_test.go:43-46`).
  `resource_drift`: **zero occurrences** anywhere. GH action `terraform-drift-report` pins
  `github:sethbacon/terraform-drift-contract#v1.0.0` (unscoped; debt).

## 4. Design decisions (fixed)

1. **Dispatch stays the model.** Push/ingest is unchanged and remains available.
2. **Unit of work = repo.** A `DriftTarget` may carry `targets[]`; a fan-out-capable
   pipeline receives them all in one ADO run and reports one callback **per target** using
   that target's own run id + one-shot token. Callback handler, reconciler and records are
   untouched — every run remains an independent row with its own token and TTL.
3. **Wire back-compat is absolute.** A request without `targets` — or with exactly one —
   produces exactly today's 3-parameter body; the run's `batch_id` is NULL and
   `schedules.last_run_id` stays a run id. `targets` is only sent for 2+ items to
   connections flagged `fan_out: true`. `DispatchAzureDevOps` keeps its signature.
4. **Pacing lives in TSM**: per-tick `LIMIT` + a global in-flight cap. Spread is a
   scheduling convention applied by the onboarding script (hash-based minute offsets).
5. **Coverage is computed, not stored**: `ListStates` (blob truth) joined with
   latest-run-per-state, live record and schedule membership.
6. **Contract change is additive and last** (Phase 5), gated on the corpus.

## 5. Phases

Phase order is a dependency order. Phases 1, 2 and 4a (backend) are independently mergeable
behind additive schema. Phase 3 depends on 1. Phase 4b (frontend) depends on 1 + 4a.
Phase 5 is independent but sequenced last.

### Spike results (run 2026-09-05 against `bconline/Brunswick`, pipeline 3642)

Method: the ADO **preview** API (`POST …/pipelines/{id}/preview?api-version=7.1-preview.1`)
compiles a run and returns `finalYaml` without queuing anything. A throwaway branch
carried the spike YAML; it was deleted afterwards. `yamlOverride` needs `EditBuild`, so the
YAML must live in the repo.

- [x] 1.0(a) **Confirmed.** A JSON *string* in `templateParameters.targets` is coerced into
  the `type: object` parameter and `${{ each t in parameters.targets }}` expands one step
  set per item. A JSON *array value* is **rejected** (`400 Value cannot be null: runParameters`)
  — values must be strings, which is what `DriftInputs.TargetsJSON` sends. An undeclared
  parameter is `400 Unexpected parameter`; an empty `targets` compiles the legacy path only.
- [x] 1.0(b) **Tokens are visible.** Every `${{ t.callback_token }}` reference is expanded
  verbatim into `finalYaml` (`env: CB_TOKEN: <value>`). Today's single `callback_token`
  template parameter is expanded the same way, so this is the same exposure *in kind*
  (one-shot tokens, private network, build-read audience) and larger *in volume*. A
  `type: string` parameter is expanded identically — **the "string fallback" is not a
  mitigation**; runtime `issecret=true` masking cannot hide a compile-time expansion.

**Recommendation (fan-out path only; the legacy 3-parameter contract stays as is):** pass
per-target tokens as **secret run variables** in the Runs API body —
`"variables": {"cb_token_<safe_dir>": {"value": "<token>", "isSecret": true}}` — and have
the `fan-out` template reference `$(cb_token_${{ replace(t.working_dir, '/', '_') }})` in
the drift task's `callbackToken` input. The macro *name* is composed at compile time, the
secret resolves at run time, so `finalYaml` and the Parameters view show only the
reference. `targets` keeps `working_dir` / `state_key` / `callback_url` (non-secret).
Needs one real run to confirm the task receives the value; not yet done.

**Identity finding for Phases 1b/3:** an Entra token minted with
`az account get-access-token --resource 499b84ac-…` under `sbacon_ca@brunswick.com`
reaches the Pipelines APIs (list/preview `200`; no `EditBuild`) but sees **zero Git repos**
org-wide. `seth.bacon@brunswick.com` (the identity behind the cached Git credential) has
repo access. Name the identity the onboarding script runs as, and grant it Code Read +
Contribute explicitly — pipeline rights do not imply repo rights.

### Phase 0 — Environment prerequisites (ops, no code) — VERIFIED 2026-09-06, PARTIALLY MET

Verified by live reads on 2026-09-06: Azure DevOps REST as `seth.bacon` (Agent Pools,
Elastic Pools, Queues, Entitlements), Azure as `sbacon_ca` (Resource Graph, Compute Gallery,
Managed Identity), and the TSM API at `tfstate.brunswick.com` with the admin key.

- [ ] **Dedicated drift agent pool — NOT MET, and now known to be mandatory.** The pool
  every template uses, `ubuntu-minimal-scale-set` (ADO pool **68**, Brunswick queue
  **1358**), is an elastic pool over `VMSS-ENT-APP2975-P-CUS07` (subscription
  `be7ae9c6-…`, `RG-ENT-APP2975-P-CUS01`, `Standard_D2ds_v5`, **maxCapacity 4**,
  desiredIdle 2, recycle-after-each-use, TTL 30 min). Siblings: `ubuntu-scale-set` (pool 51
  → CUS05, max 4), `windows-scale-set` (43 → CUS03, max 8), `windows-scale-set-custom`
  (64 → CUS06, max 3). With the real ceiling of **4 Linux agents**, the capacity math in §1
  becomes: per-app jobs ≈ 4,500 agent-min ≈ **19 h**; per-repo fan-out ≈ 2,100 agent-min ≈
  **8.75 h** — neither fits a night, and either would starve every deploy that shares the
  pool. Action: create `ubuntu-drift-scale-set` — a new VMSS from the **same gallery image
  definition** (below), its own elastic pool with `recycleAfterEachUse`, desiredIdle 0,
  TTL 30, max sized to the nightly window (max 20 ⇒ fan-out ≈ 1.75 h), plus a Brunswick
  queue — and point the drift templates at it via `pool`.
- [x] **Internal CA in the agent image — ALREADY MET.** The agent image is Compute Gallery
  `ACG_ENT_APP6030_P_CUS01` (`RG-ENT-APP6030-P-CUS01`, the image factory) definition
  `Canonical-UbuntuMinimal-24_04-LTS-gen2`, version `2026.08.26` (three versions in
  August 2026 — `2026.08.17/.25/.26` — each published from a managed image
  `ubuntu-24_04-lts-minimal-<date>` in the same resource group, i.e. a dated image
  pipeline, not hand-built). `BCROOT` is in that image's trust store: the June end-to-end callback
  validated `tfstate.brunswick.com` with TLS verification on (no `-k`, no
  `rejectUnauthorized:false`) once the *server* chain carried the 2024 `BCIssuingCA1`. The
  served chain was re-checked today: leaf ← `BCIssuingCA1` ← `BCROOT`, leaf valid to
  2028-06-15. No image change needed; the drift VMSS must use this same definition.
- [x] **TSM callback base — MET.** `GET /api/v1/pipelines/callback-preflight` returns
  `{"callback_base":"https://tfstate.brunswick.com","likely_unreachable":false}`, and the
  private DNS record resolves on the scale-set agents (the templates' `/etc/hosts` pin is a
  no-op). The preflight heuristic (`ci_sources.go:568-592`) is advisory only for a private
  deployment.
- [ ] **Credentials — PARTIAL.**
  - TSM admin API key for tooling: present (used for these reads). ✓
  - ADO identity for TSM dispatch (Phase 1b): the pod identity
    `terraform-state-manager-identity` (`RG-ENT-APP6637-N-CUS01`, client id
    `0c48ae6c-…`) is federated to **`AKS-ENT-APP663701-N-CUS01`** (OIDC issuer +
    Workload Identity enabled; subject
    `system:serviceaccount:terraform-suite:terraform-state-manager`), so
    `auth_method: workload_identity` is deployable as designed. But **no managed identity
    is an Azure DevOps org user yet** (entitlement search for it: 0 matches). Action: create
    `tsm-ado-dispatch` (UAMI + federated credential on the same subject) or reuse the pod
    identity; add it to `bconline` (Basic); grant Build *Read & execute*, Project and Team
    *Read*, Code *Read* on Brunswick.
  - Operator identity split (confirmed by the owner): **`sbacon_ca` = Azure (`az`)**,
    **`seth.bacon` = Azure DevOps**. Two traps found: the Git Credential Manager token for
    `seth.bacon` is *Git-scoped* (repos work; Pipelines/Agent Pools/Entitlements 401), and
    an Entra token minted under `sbacon_ca` reaches Pipelines but sees no repos. The
    onboarding script (Phase 3) therefore needs a `seth.bacon` PAT with Build + Code +
    Agent Pools (read) scopes — or `az login` as `seth.bacon` — until the Phase 1b identity
    exists.
  - Resource authorization (queue 1358, service connection, variable groups) was granted to
    the three test definitions (3642/3643/3645) and still holds; every new definition needs
    the same `pipelinePermissions` grant — that is the onboarding script's job.
- [ ] **TSM deployment — NOT MET (new item).** `tfstate.brunswick.com` runs **3.20.1**
  (built 2026-09-02), which predates fan-out / workload identity (#569) and the follow-ups
  (#571); release 3.21.0 (release-please #568) is not merged or deployed. The instance is
  also **empty** — 0 state sources, CI sources, pipeline connections, schedules, runs — so
  the June test wiring was lost in a redeploy. Action: merge #568, deploy 3.21.x through
  pipeline 3632 (`terraform-state-manager-aks`), then re-register one state source
  ("Azure Non-Prod", `crpnonprdtrrfrmstaterepo/terraformbackend`), the CI source (Phase 1b
  identity, `workload_identity`) and one fan-out connection before the acceptance dispatch.

**Done when** (unchanged): a manual dispatch from TSM to a pipeline on the new pool completes
a callback with TLS verification on and no host pinning. **Gating today: the drift pool, the
ADO identity, and the 3.21 deployment.**

### Phase 1 — Repo-level fan-out dispatch (backend + templates) — DONE, except 1.4's Brunswick templates

#### 1.0 Spike (½ day; blocks 1.3/1.4) — one throwaway pipeline, two questions

- (a) **Object-param coercion.** YAML declares
  `parameters: [{name: targets, type: object, default: []}]` and prints
  `${{ each t in parameters.targets }}` → `${{ t.working_dir }}`; call
  `POST …/_apis/pipelines/{id}/runs?api-version=7.1` with
  `templateParameters: {"targets": "[{\"working_dir\":\"A\"},{\"working_dir\":\"B\"}]"}`.
  **Fallback if coercion fails:** `targets` becomes `type: string`; TSM ALSO sends
  `working_dirs` (comma-joined) so the template can
  `${{ each d in split(parameters.working_dirs, ',') }}`; per-target callback URL/token come
  from a runtime step that parses the `targets` JSON with `python3` (present on the minimal
  agent; `jq` is not) and sets `##vso[task.setvariable variable=cb_url_<APP>]` /
  `cb_token_<APP>;issecret=true` before the loop.
- (b) **Token exposure under `${{ each }}`.** Pass a dummy token per target and inspect the
  run's *expanded YAML* and Parameters view. Today's single `callback_token` is a
  compile-time parameter too, so exposure is the same **in kind** (one-shot token, private
  network, build-read audience) and only larger **in volume**. If the expanded YAML shows
  values, the mitigation is the fallback in (a) (tokens read at runtime from the JSON string
  and masked immediately), not a design change.

#### 1.1 Migration `000033_drift_runs_batch` — `backend/internal/db/migrations/`

```sql
-- up
ALTER TABLE drift_runs
    ADD COLUMN IF NOT EXISTS batch_id   UUID,
    ADD COLUMN IF NOT EXISTS ci_run_id  TEXT,
    ADD COLUMN IF NOT EXISTS ci_run_url TEXT;
CREATE INDEX IF NOT EXISTS idx_drift_runs_batch
    ON drift_runs (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_drift_runs_state_created
    ON drift_runs (source_id, state_key, created_at DESC);
-- down: DROP INDEX IF EXISTS (both); ALTER TABLE drift_runs DROP COLUMN IF EXISTS (all three)
```

Nullable columns with no default → metadata-only, no table rewrite. Plain `CREATE INDEX`:
golang-migrate executes each file as one exec, and Postgres runs a multi-statement exec in an
implicit transaction, so `CONCURRENTLY` would fail unless it were the *only* statement in its
own file; `drift_runs` is small at rollout. Note in `docs/upgrade-guide.md` that an operator
with a very large `drift_runs` (>1M rows) should pre-create the two indexes `CONCURRENTLY`
by hand before upgrading. Update the stale "next migration" note in `CONTRIBUTING.md:221`.

#### 1.2 Repository — `internal/db/repositories/drift_repository.go`

- `DriftRun` += `BatchID *string` (json `batch_id`), `CIRunID string` (json `ci_run_id`),
  `CIRunURL string` (json `ci_run_url`); extend `driftColumns`, the scanner and `Create`.
- New `DriftRunFilter{Status, BatchID, SourceID, StateKey string}`; change
  `List(ctx, limit, offset int, f DriftRunFilter)` and `CountRuns(ctx, f DriftRunFilter)`;
  add `CountRunsIn(ctx, statuses []string) (int, error)`; update all call sites.
  The `BatchID` filter SQL is `(batch_id = $n OR id = $n)` (see 1.3).
- New `SetCIRun(ctx, batchOrRunID, ciRunID, ciRunURL string) error`
  (`UPDATE drift_runs SET ci_run_id=$2, ci_run_url=$3 WHERE batch_id=$1 OR id=$1`).
- New `FailBatch(ctx, batchID, detail string) error`
  (`UPDATE … SET status='failed', detail=$2, callback_token='' WHERE batch_id=$1 AND status='dispatched'`).
- Tests (sqlmock): `TestDriftRepository_Create_SetsBatchID`, `TestDriftList_FilterBatchOrRunID`,
  `TestDriftList_FilterSourceState`, `TestCountRunsIn`, `TestSetCIRun_OnlyAffectsGivenBatch`,
  `TestFailBatch_OnlyDispatched`.

#### 1.3 Dispatch — `internal/api/drift.go`, `internal/pipelines/azuredevops.go`, `github.go`

- `DriftTargetItem{SourceID, StateKey, WorkingDir string}`; `DriftTarget` += `Targets []DriftTargetItem`
  (json `targets`). Helper `func (t DriftTarget) items() []DriftTargetItem` returns
  `Targets` if non-empty else the single legacy triple — **one code path**.
- Validation (in `CreateRun` and `scheduleRequest.validate()`): each item through
  `validatePipelineInputs`; `len(items) <= 100`; no duplicate `(source_id, state_key)`
  within the request. **Fan-out gate is `len(items) > 1`** — a one-item `targets` array is
  *defined* as the legacy single path. For `> 1` the connection's `config.fan_out` must be
  `true` → else `400 {"error":"pipeline connection is not fan-out capable"}`.
- **`fan_out` is validated at write time**: `CreatePipeline`/`UpdatePipeline`
  (`drift.go:104-186`, next to the `Provider` check at :116) reject a `config.fan_out` that
  is present but not a JSON boolean (`400 "config.fan_out must be a boolean"`). Reader
  `FanOutFromMap(cfg map[string]any) bool` does a strict `bool` type-assert.
- `dispatchDrift` → `dispatchDriftBatch(ctx, tgt, actor) (*DriftBatch, error)`,
  `DriftBatch{BatchID string; Runs []*repositories.DriftRun}`. **`batch_id` semantics:**
  for `len(items) == 1` the row's `batch_id` stays **NULL** and `DriftBatch.BatchID = run.ID`
  (so `schedules.last_run_id` remains a real run id for every non-fanned schedule, as the
  FE assumes — `SchedulesPage.test.tsx:36`); for `> 1`, `batch_id = uuid.NewString()` on
  every row. Steps: create one run per item (own `randomToken()`); build `targets` JSON
  `[{"working_dir","state_key","callback_url","callback_token"}]`; params = legacy three
  from item[0] **plus `"targets"` only when `len > 1`**; dispatch once; on error
  `FailBatch`/`UpdateStatus` and return `(batch, err)`; on success call `SetCIRun`
  **best-effort** — log and continue if it errors (CI has already started; failing the HTTP
  response would desync run status from reality). `ci_run_id/ci_run_url` are therefore
  nullable even on successful dispatch. Blank all `CallbackToken`s before returning.
- **Do not change `pipelines.DispatchAzureDevOps`'s signature** — shared with Version Lab
  (`internal/api/health.go:137`) and `dispatch_test.go:96-190`. Add
  `DispatchAzureDevOpsRun(ctx, cred, cfg, ref, params) (*CIRunRef, error)` that does the
  POST and decodes `{"id":…, "_links":{"web":{"href":…}}}` into
  `CIRunRef{ID string; WebURL string}` (a missing/malformed body on 200/201 → `(nil, nil)`,
  never an error); make the existing `DispatchAzureDevOps` a thin wrapper that discards the
  ref. `DispatchAzureDevOpsDrift(…, inputs DriftInputs) (*CIRunRef, error)` calls the new
  function; `DriftInputs` += `TargetsJSON string` (sent as `"targets"` only when non-empty).
  GitHub: `DispatchGitHubDrift` returns `(nil, nil)` on 204.
- `CreateRun` response: no `targets` (or one item) → `202` single `DriftRun` (same keys as
  today plus `batch_id: null`, `ci_run_id`, `ci_run_url`); `> 1` →
  `202 {"batch_id":…, "runs":[…]}`. Audit `drift_run.dispatch` metadata += `batch_id`,
  `targets: n`.
- `ListRuns` accepts `batch_id`, `source_id`, `state_key` query params → `DriftRunFilter`.
  Swagger annotations updated on `CreateRun`, `ListRuns`; `make swag`.
- `driftDispatcher.Dispatch` returns `batch.BatchID` as `runID` (= run id when single,
  batch uuid when fanned). Schedules created before this change unmarshal unchanged.
- `notifyDriftResult` message includes `state_key` and `ci_run_url` when set. **N
  notifications per batch is intentional** (one per state = one per record).
- **Concurrency is unchanged**: two runs on the same `(source_id, state_key)` remain
  independent rows; `UpsertDetection`/`ResolveClean` stay last-write-wins. State this in a
  code comment on `dispatchDriftBatch`; the onboarding script prevents duplicates by
  construction.
- Tests — `dispatch_test.go`: `TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly`
  (golden: exactly `callback_url,callback_token,working_dir`),
  `TestDispatchAzureDevOps_WireBody_WithTargets_AddsParam`,
  `TestDispatchAzureDevOpsRun_DecodesRunIDAndWebLink_On201`,
  `TestDispatchAzureDevOpsRun_MalformedBody_NilRefNoError`; keep
  `TestDispatchAzureDevOps_UnexpectedParameterHint`; the health call site compiles untouched.
  `drift_batch_http_test.go` (rig `newDriftEnv`):
  `TestCreateRun_LegacySingle_ResponseKeysUnchangedPlusNullBatchID`,
  `TestCreateRun_TargetsSingleItem_FanOutFalse_Allowed_NullBatchID`,
  `TestCreateRun_TargetsMultiple_FanOutFalse_400`,
  `TestCreateRun_TargetsMultiple_FanOutTrue_202BatchShape_OneDispatch`,
  `TestCreateRun_Targets_DuplicateSourceStateKey_400`, `TestCreateRun_Targets_Over100_400`,
  `TestCreateRun_DispatchFails_AllRunsInBatchFailed`, `TestCreateRun_SetCIRunError_StillAccepted`,
  `TestRunResults_PerRun_AfterBatchDispatch`, `TestListRuns_FilterBatchIDMatchesBatchOrRunID`,
  `TestGetRun_ExposesBatchIDNotToken`, `TestCreatePipeline_FanOutMustBeBoolean_400`.
  `schedules_dashboard_http_test.go`: `TestSchedule_TargetsValidated`,
  `TestRunSchedule_LastRunIDIsRunIDWhenSingle`, `TestRunSchedule_LastRunIDIsBatchIDWhenFanned`,
  `TestDriftDispatcher_LegacyTargetConfigWithoutTargets`.

#### 1.4 Templates (Azure DevOps YAML)

- **Built-in profile `fan-out`** for `azure_devops/drift` in `drift_workflows.go` (new const
  `azureDriftPipelineFanOut`, seeded via `builtinWorkflowSeeds`; add to
  `drift_workflows_test.go` conformance: declares `callback_url`, `callback_token`,
  `working_dir`, `targets`). Structure: install/mirror once → `${{ each t in parameters.targets }}`:
  mask token (`##vso[task.setvariable variable=cb_<dir>;issecret=true]${{ t.callback_token }}`)
  → init (backend key from `t.state_key`) → plan (`continueOnError: true`,
  `-detailed-exitcode -out`) → `terraform show -json` → `PipelineTerraformDriftReport@1`
  (`callbackUrl: ${{ t.callback_url }}`, `callbackToken: ${{ t.callback_token }}`) → marker
  step `##vso[task.setvariable variable=reported_<dir>]true` → failure-report step
  `condition: and(always(), ne(variables['reported_<dir>'], 'true'))` POSTing
  `{"status":"failed"}` with that target's token (409-tolerant) → workspace reset
  `git checkout -- . && git clean -fd -e .terraform` (keep the provider cache). Every
  per-target step `continueOnError: true` so one broken app does not stop the rest. When
  `targets` is empty the template behaves exactly like today
  (`${{ if eq(length(parameters.targets), 0) }}` guard).
- **Failure semantics (state in the template header comment):** an app whose own steps ran
  but failed gets an immediate `failed` via its failure-report step; apps whose steps were
  **never scheduled** (job cancelled/timed out after app k) stay `dispatched` until the
  reconciler expires them at `TSM_DRIFT_RUN_TTL` (default 2h). This is intentional — the
  reconciler is the backstop. Operators with short drift jobs may lower `TSM_DRIFT_RUN_TTL`
  to ~1h; do not add job-level cleanup steps for it.
- **Brunswick operator templates**: port the same loop into
  `azure-pipelines-tsm-drift.brunswick-azure-ext.yml` (module downloads :209-245 and
  installer :249 move *before* the loop; resolver/assemble :85-151, init :255, plan :270,
  show :296, report :326, failure :342 become the loop body with `${{ t.working_dir }}`
  replacing `parameters.working_dir`), then `…TBD4826-ext.yml` and
  `…brunswick-oci-ext.yml`. Re-run `upload-templates-to-tsm.ps1`. Validate with a
  `dry_run: true` dispatch against TBD4330 (def 3642) using two apps (APP5849 clean +
  APP5848 drift) and one deliberately broken app to prove per-target isolation.

**Done when:** one ADO run for TBD4330 with 3 targets produces 3 `drift_runs` rows under one
`batch_id`, each `completed`/`failed` correctly, `ci_run_url` populated, and a request
without `targets` produces a byte-identical wire body to today (golden test).

### Phase 1b — Identity between TSM, Azure DevOps and the cloud (no PATs, no per-app rows) — DONE except item 3 (per-target `Params`)

**Entity model after Phase 1 (answers the "a source per app" objection):**

| Entity | Count | Holds a credential? |
| --- | --- | --- |
| `ci_source` (ADO org/project) | **1** | yes — the only TSM→ADO identity |
| `pipeline_connection` (one per repo) | ~300 | **no** — borrows via `config.ci_source_id` |
| `schedule` (one per repo, `targets[]`) | ~300 | no |
| target (one per app) | ~1,500 | no row of its own; lives inside the schedule |
| `state_source` (one per storage container) | a few | yes — backend read (unchanged) |

Nothing per app is created in TSM. Cloud credentials never enter TSM at all.

**Three hops, three identities, zero shared secrets:**

1. **TSM → ADO (dispatch + discovery): AKS Workload Identity, `auth_method: workload_identity`.**
   - *Azure:* create a dedicated user-assigned managed identity `tsm-ado-dispatch` and a
     federated credential on it with the chart ServiceAccount subject
     (`system:serviceaccount:<ns>:<release>-terraform-state-manager`, same recipe as
     `docs/deployment/aks-prerequisites.md:132-143`). One SA token can be exchanged for any
     identity that federates that subject, so this stays separate from the Key Vault identity.
   - *ADO:* Organization settings → Users → add the managed identity (access level **Basic**);
     then the same minimum set the existing runbook documents for app registrations
     (`docs/deployment/ado-app-registration.md` §2): **Build: Read & execute** (dispatch)
     and **Project and team: Read** (discovery/verify), plus **Code: Read** on the repos the
     discovery lists enumerate. **No `Contribute`** — TSM never writes to repos in this
     design (the onboarding script commits YAML under the operator's own identity, see 4
     below). Managed identities have been first-class ADO users since 2023; the token
     audience is the fixed ADO resource `499b84ac-…`.
   - *Docs:* extend `docs/deployment/ado-app-registration.md` with a "Managed identity
     (Workload Identity)" section and troubleshooting rows (federated-token exchange
     `AADSTS70021`/`AADSTS700213`; "identity is not a member of the organization") rather
     than adding a new runbook; keep its "When to use which" table as the decision aid.
   - *Credential replacement in place:* add `PUT /api/v1/ci-sources/{id}` (`sources:manage`,
     audited `ci_source.update`) accepting `auth_method` + the matching secret shape, so an
     operator can move the single `bconline` source `app` → `workload_identity` (or rotate a
     secret) **without deleting it** — today the routes are `GET/POST/DELETE/verify` only
     (`router.go:445-459`), and a delete orphans every `pipeline_connection` that borrows the
     source via `config.ci_source_id`. Re-encrypt on write; invalidate the minter cache entry
     (`ResetEntraTokenCacheForTest`-style keyed eviction, `entra.go:43`). The runbook's §4
     "Rotation" step currently describes an operation that has no endpoint — fix the doc when
     the route lands.
   - *Backend:* migration `000035_ci_sources_workload_identity` — extend the `auth_method`
     CHECK and `ci_sources_auth_shape` (`workload_identity` requires `client_id` only; no
     encrypted columns). New `pipelines.MintWorkloadIdentityADOToken(ctx, clientID)` using
     `azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{ClientID})`
     → `GetToken(scopes ["499b84ac-1321-427f-aa17-267ca6975798/.default"])`, cached with the
     same fingerprint + 60 s refresh margin as `MintEntraADOToken` (`internal/pipelines/entra.go:80-117`).
     (`azidentity` is a new direct dependency; `azcore` is already indirect.) Branch on the
     new method in `sourceToken`/`resolvePipelineToken` (`internal/api/ci_sources.go:287-291`)
     and `VerifyCISource` (`:267`) — always `Bearer`. `CreateCISource` (`:158-211`) accepts
     the new value; `ciSourceJSON` renders it.
   - *Frontend:* `src/pages/drift/CISourcesDialog.tsx` auth-method toggle gains
     `workload_identity` (ADO only; single field `client_id`; helper text pointing at the
     deployment doc).
   - *Fallback* where Workload Identity is unavailable (non-AKS installs): the existing
     `app` method (client secret, stored AES-256-GCM, rotated via Key Vault). **`pat` is
     never used for TSM.**
   - *Optional refactor while here:* the two earlier auth plans agreed on a shared
     `TokenMinter` abstraction that was never built — `sourceToken` branches on
     `auth_method` strings (`ci_sources.go:287-291`) and `ADOToken{Bearer bool}`
     (`pipelines/ado_auth.go:13`) is the only shared type. A third branch is acceptable; a
     small `type adoTokenSource interface{ Token(ctx) (string, error) }` keyed by
     `auth_method` is cleaner but not required for this plan.
   - Tests: `TestMintWorkloadIdentityADOToken_CachesAndRefreshes` (fake `TokenCredential`),
     `TestCreateCISource_WorkloadIdentity_RequiresClientIDOnly`,
     `TestResolvePipelineToken_WorkloadIdentity_Bearer`, `TestVerifyCISource_WorkloadIdentity`,
     `TestUpdateCISource_SwitchesAuthMethodAndEvictsCache`,
     `TestUpdateCISource_PreservesConnectionsBorrowingSource`.
   - Housekeeping: both adjacent plans (`drift-ado-app-registration-auth.md`,
     `drift-github-app-auth.md`) are fully implemented, runbooks included, but still carry
     `Status: Proposed` — flip them to `Implemented` in the same PR.

2. **ADO → TSM (results): per-run one-shot tokens — unchanged.** N tokens per fan-out run,
   each bound to one run id, consumed atomically (409 on replay), expired by the reconciler.
   TLS to the private FQDN with the internal CA baked into the agent image (Phase 0). No
   standing credential exists in this direction.

3. **Pipeline → cloud (the plan itself): the repo's own WIF service connections, per app.**
   No secrets, no shared fleet credential, and no TSM knowledge of cloud identities.
   Service-connection inputs must be resolvable at compile time, so the per-app SC name
   travels inside the `targets` object: `DriftTargetItem` += `Params map[string]string`
   (opaque pass-through; keys/values validated with the existing `^[A-Za-z0-9._/\-]*$`
   regex, ≤ 8 keys) and the template binds `${{ t.params.service_connection }}`. The
   onboarding script fills it from `pipeline-parameters.yml` and authorizes each SC for the
   drift pipeline definition (`pipelinePermissions` PATCH). Per-app identity also removes
   the shared-SC artefacts seen on TBD4826 (the `data.azurerm_client_config` access-policy
   false positive and Key Vault 403s).

4. **Operators and scripts → ADO / TSM: no PAT either.** `onboard_drift.py` obtains an ADO
   bearer from the operator's own login —
   `az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798` — replacing
   `AZUREDEVOPS_PAT` in the Brunswick scripts. Toward TSM it uses an API key scoped
   `sources:manage` + `state:drift` (admin scope only for template-registry uploads).

*Follow-up outside this plan:* Azure blob **state sources** still authenticate with
`account_key` (`internal/statesource/azure.go:37-47`); once `azidentity` is in the tree, an
Entra/Workload-Identity option for the blob connector (mirroring the pipelines'
`backendAzureRmUseEntraIdForAuthentication`) is a natural next step.

**Done when:** the `ci_source` for `bconline/Brunswick` has `auth_method=workload_identity`,
"Test connection" passes, a dispatch succeeds with no secret material stored in TSM, and a
`git grep -i pat` over the Brunswick onboarding scripts finds nothing.

### Phase 2 — Scheduler pacing (backend) — DONE

- Config: `DriftConfig` += `MaxInFlight int` (`TSM_DRIFT_MAX_IN_FLIGHT`, default `0` =
  unlimited); new `SchedulerConfig{BatchLimit int}` (`TSM_SCHEDULER_BATCH_LIMIT`, default
  `50`) in `config.go` + `setDefaults`.
- `ScheduleRepository.GetDue(ctx, now, limit int)` → `… ORDER BY next_run_at LIMIT $2`.
  `scheduler.New(repo, dispatcher, opts Options{Interval, BatchLimit, MaxInFlight, InFlight func(ctx) (int, error)})`.
- In `fire`, **before** `ClaimDue`: if `MaxInFlight > 0` and `InFlight() >= MaxInFlight` →
  log `"in-flight cap reached; deferring"` and return **without claiming** (the row stays
  due and is retried next tick; no starvation because runs complete or expire).
  `InFlight` = `CountRunsIn(ctx, []string{"dispatched","running"})`.
- Metrics (`telemetry/metrics.go`): `tsm_drift_runs_in_flight` (gauge, set each tick),
  `tsm_scheduler_due_backlog` (gauge = due but unclaimed after the tick),
  `tsm_drift_dispatch_total{result="ok|failed|deferred"}` (counter),
  `tsm_drift_records_open{severity}` (gauge refreshed on the reconciler tick). Document in
  `docs/observability.md`.
- Tests: `scheduler_test.go` — `TestCheckDue_RespectsBatchLimit`,
  `TestFire_DefersWhenInFlightCapReached_NoClaim`, `TestFire_ProceedsUnderCap`;
  `schedule_notify_edit_test.go` — `TestGetDue_Limit`.

**Done when:** with `TSM_DRIFT_MAX_IN_FLIGHT=20` and 60 due schedules, no more than 20 runs
are ever `dispatched|running` at once and all 60 fire within (60/20) × tick with every
schedule's `last_run_at` advanced exactly once.

### Phase 3 — Discovery-driven bulk onboarding (Brunswick tooling) — NOT STARTED

New `C:\dev\ado\code\shared\scripts\python\Azure\migration_scripts\drift\onboard_drift.py`
(+ `README.md`), dry-run by default, idempotent by name. Reuse `select_repos.py`
`api()`/PAT auth, `oci_decompose.parse_active_tops()` for active stages, and
`.tsmbase`/`.tsmtoken` (or `TSM_API_KEY`) for TSM.

Per repo (input: repo names or `--all-tbd`):

1. Read `pipeline.yml` + `pipeline-parameters.yml`; enumerate **active** apps (`APP_ID_*`
   whose stage is not commented out — stale apps plan-fail, cf. TBD4330/APP5851).
2. Choose profile by provider (`brunswick-azure-ext`; `-sql-ext` if
   `terraform_sqldb_azure_module` present; `brunswick-oci-ext`), state source id by env
   (nonprod/prod), state key `APP####.tfstate` or `oci/APP####.tfstate`; **verify each key
   exists** via `GET /api/v1/sources/{id}/states`; report missing keys instead of
   registering them (a target without `source_id`+`state_key` records nothing).
3. Fetch template `GET /api/v1/drift/workflow?provider=azure_devops&profile=<p>`; commit as
   `azure-pipelines-tsm-drift.yml` via
   `POST /api/v1/ci-sources/{id}/repos/{repo}/workflow-setup` (opens a PR) — or
   `--direct-branch` for a designated test branch.
4. Create the ADO pipeline definition `POST /api/v1/ci-sources/{id}/repos/{repo}/pipelines`
   (skip if a definition with the name exists — mirror the wizard's `adoUseExisting`).
5. Create the connection `POST /api/v1/pipelines` with
   `config: {organization, project, pipeline_id, ref, ci_source_id, fan_out: true}`.
6. Create ONE schedule `POST /api/v1/schedules` with
   `target_config: {pipeline_connection_id, targets:[…]}`, cron `"<minute> <hour> * * *"`
   where `minute = crc32(repo) % 60` and `hour` is spread across `--window 01-05` by hash —
   the pacing convention.
7. Emit `onboard-report.csv` (repo, apps, profile, connection id, schedule id,
   skipped-with-reason). `--verify` re-reads TSM and diffs.

**Done when:** running against TBD4330, TBD4826, TBD728 in `--apply` mode yields 3
connections + 3 schedules, `--verify` is clean, and `POST /schedules/{id}/run` on each
produces one batch with N completed runs.

### Phase 4 — Dashboard read-path — DONE (4a + 4b)

#### 4a Backend

- `GET /api/v1/drift/coverage?source_id=<id>&stale_after=24h` (scope `state:read`): for each
  `StateRef` from the connector, join latest run (new
  `DriftRepository.LatestPerState(ctx, sourceID) (map[string]DriftRun, error)` —
  `SELECT DISTINCT ON (state_key) … ORDER BY state_key, created_at DESC`), live record
  (`DriftRecordRepository.LiveByState(ctx, sourceID)`), and schedule membership
  (`ScheduleRepository.TargetsIndex(ctx)` — parse `target_config` in Go). Response
  `{states:[{key, scheduled, last_run_id, last_run_at, last_status, drifted, unparseable, truncated, ci_run_url, record_id, record_status, severity}], summary:{total, scheduled, unscheduled, stale, incomplete, open, critical}}`.
  Cache per source for 60 s (simple in-memory TTL map).
- `GET /api/v1/drift/summary` (scope `state:read`):
  `{records_by_source:[{source_id, source_name, open, acknowledged, critical}], runs_24h:{completed, failed, dispatched}, incomplete_records, in_flight}`.
- Retention: `DriftRetentionConfig{Enabled bool; KeepPerState int; MaxAge, ResolvedMaxAge time.Duration}`
  (`TSM_DRIFT_RETENTION_*`, defaults `true/20/90d/180d`);
  `DriftRepository.PruneRuns(ctx, keepPerState, maxAge)` copying `PruneBackups`' window
  pattern partitioned by `(source_id, state_key)`;
  `DriftRecordRepository.PruneResolved(ctx, maxAge)`; run once per reconciler tick
  (`driftreconcile` gets an optional `Pruner` hook) — no new goroutine.
- Tests: `drift_coverage_http_test.go` (`TestCoverage_JoinsRunRecordSchedule`,
  `TestCoverage_StaleAndUnscheduled`, `TestCoverage_SourceNotFound404`),
  `TestDriftSummary_Grouping`, repository `TestLatestPerState_DistinctOn`,
  `TestPruneRuns_KeepsNewestPerState`, `TestPruneResolved`. Swagger + `make swag`.

#### 4b Frontend

- `api.ts`: `DriftRun` += `batch_id`, `ci_run_id`, `ci_run_url` and the five completeness
  fields (`truncated, omitted_entries, omitted_attrs, unparseable, unmasked`) — also on
  `DriftRecord`; `listDriftRuns` params += `batchId, sourceId, stateKey`; new
  `getDriftCoverage(sourceId, staleAfter?)`, `getDriftSummary()`; `ScheduleTargetConfig` +=
  `targets?: {source_id, state_key, working_dir}[]`; `CreateDriftRunInput` += `targets?`.
  `queryKeys.drift` += `coverage(sourceId)`, `summary()`.
- **Completeness badges** (highest value, smallest change): a `CompletenessChips` component
  (`unparseable` → error chip "not verified"; `truncated` → warning "partial: N entries /
  M attrs omitted"; `unmasked` → warning) rendered in `DriftRunDetailDialog` (:49-57), the
  record-detail chips row (`DriftRecordsSection.tsx:399-411`) and as an icon in the
  runs/records tables. Also render `acknowledged_at`/`resolved_at` (already fetched) and an
  "Open CI run" link when `ci_run_url` is set.
- **Coverage tab** on `DriftPage`: source picker → table (state key, scheduled?, last
  checked, status, drifted, completeness, record status, CI link) with filters
  `unscheduled | stale | incomplete | drifted`; summary chips from `summary`.
- **Schedules**: `ScheduleFormDialog` gains a **targets repeater** (rows of source→state
  Autocomplete + working dir; "add all states matching /regex/" helper using `listStates`),
  shown when the selected connection has `config.fan_out`. Pipeline picker → MUI
  `Autocomplete` here and in `NewRunDialog`. Schedules table links `last_run_id` to
  `/drift?batch=<id>` (works for single and fanned schedules via the `batch_id OR id` filter).
- **Wizard**: profile picker (from `GET /admin/ci/templates` filtered `azure_devops/drift`
  when admin; else `default`/`suite`/`fan-out`), `fan_out` checkbox writing `config.fan_out`.
- **Landing page cards** (`LandingPage.tsx:144-163` pattern): "Open drift", "Critical",
  "Stale checks", "Unverified" from `getDriftSummary`.
- i18n: new keys under `pages.drift.*` / `pages.schedules.*` in `en/translation.json` only.
- Tests (vitest): `CompletenessChips.test.tsx`, `DriftCoverageTab.test.tsx`,
  `ScheduleFormDialog.targets.test.tsx`; extend `DriftRunDetailDialog`/`DriftRecordsSection`
  tests for badges + CI link; keep thresholds (85/80/82/87).

**Done when:** an operator can, from `/drift`, see for a source which states have no
schedule, which have not been checked in 24 h, and which last "clean" result was actually
unparseable — and click through to the ADO run.

### Phase 5 — Contract: separate infra drift from unapplied changes (coordinated, additive) — NOT STARTED

Order is strict: contract → Go mirror → task/action → backend storage → frontend.

1. `terraform-drift-contract` (→ **1.3.0**): `Plan` += `resource_drift?: ResourceChange[]`;
   `Result` += `drift_added, drift_changed, drift_destroyed: number` computed from
   `resource_drift` with the same skip rules (`no-op`/`read`) and bounds, plus a parallel
   `drift_summary: SummaryEntry[]` — **`summary` itself is unchanged** so existing consumers
   are untouched. Add ≥6 vectors (`drift/only-resource-drift`, `drift/both`,
   `drift/skip-read`, `drift/absent-array`, `drift/truncation`, `drift/masking`); recompute
   `CORPUS_SHA256`/`RECONCILED_DIGEST`; release-please + `publish.yml`.
2. `backend/internal/services/driftingest/plan.go`: mirror the fields; copy the new
   `vectors.json` into `testdata/conformance/`; update the two digest constants;
   `TestConformance_*` green.
3. `azure-pipelines-terraform` task: bump the dependency to `^1.3.0` (the body spread
   already forwards new fields — zero logic change); **Minor bump**
   `TerraformDriftReportV1/task.json`; release. `terraform-drift-report` action: repoint to
   `@4cloudguru/terraform-drift-contract@^1.3.0` (retire the `github:…#v1.0.0` pin),
   rebuild `dist/`.
4. Backend: migration `000034_drift_infra_counts` adding
   `drift_added/drift_changed/drift_destroyed INTEGER DEFAULT 0` + `drift_summary JSONB` to
   `drift_runs` and `drift_records`; `driftRunResultPayload`/`driftIngestPayload` += fields
   (lenient decode keeps older runners working); `UpsertDetection`/`UpdateResult` persist
   them; `drift_conformance_test.go` extended so template payload keys still match.
5. Frontend: two count triplets ("infra drift" vs "unapplied") in dialogs/tables;
   coverage/summary include `infra_drifted`.

**Done when:** a plan whose only changes are in `resource_drift` reports
`drifted:false, drift_added/changed/destroyed>0` end to end, and existing vectors are
byte-identical.

## 6. Security

- No new credential classes. N one-shot tokens per fan-out dispatch are the same token type
  as today, consumed within minutes, visible only to build-read users on a private network
  (spike 1.0(b) records the exact exposure surface).
- `fan_out` is validated at write time; `targets` is bounded (≤100) and validated with the
  existing `validatePipelineInputs` regexes.
- Callback handler, one-shot consumption, uniform-401 and reconciler are untouched.
- `ci_run_url` is stored from ADO's response, never from the callback body.

## 7. Verification (end-to-end, after Phases 1–4)

1. `cd backend && go test ./internal/... -race` green; `make swag` produces no diff; `gosec`
   baseline unchanged.
2. Frontend: `npx tsc --noEmit && npm run lint && npx vitest run` green at thresholds.
3. Dispatch golden: `TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly` proves a
   no-`targets` request sends exactly
   `{"templateParameters":{"callback_url":…,"callback_token":…,"working_dir":…}}`.
4. Live (UAT TSM + `bconline`): `onboard_drift.py --apply` on TBD4330/TBD4826/TBD728 →
   3 connections, 3 schedules; `POST /schedules/{id}/run` → one ADO run each;
   `GET /drift/runs?batch_id=` shows N rows all terminal; `GET /drift/coverage?source_id=…`
   shows those keys `scheduled=true` with fresh `last_run_at`; break one app's `.tf` → its
   run `failed` via the per-target failure step while siblings complete; kill the agent
   mid-run → remaining runs expire to `failed` via the reconciler within `RunTTL`.
5. Pacing: `TSM_DRIFT_MAX_IN_FLIGHT=2`, fire 3 schedules within a minute → the third defers
   and fires on a later tick (`tsm_drift_dispatch_total{result="deferred"}` increments).
6. UI: completeness chips visible on a run whose plan.json was replaced with `{}`
   (unparseable) — record stays open, chip reads "not verified".

## 8. Rollout

1. Phase 0 (ops) in parallel with Phase 1 code.
2. Ship 000033 + Phase 1 backend behind no flag (additive; legacy path byte-identical).
3. Publish the `fan-out` built-in and the ported Brunswick profiles; validate on the three
   proven repos before any fleet onboarding.
4. Phase 2 with `TSM_DRIFT_MAX_IN_FLIGHT` set to the drift pool's agent count.
5. Phase 3 onboarding in cohorts (10 → 50 → rest), watching `tsm_drift_runs_in_flight`
   and the coverage view.
6. Phase 4 UI can land any time after 4a; Phase 5 last.

## 9. Out of scope / follow-ups

- Bulk-registration UI; batch notification digest; per-org re-check authorization.
- `/drift/ingest` mode in the ADO task (only for repos that cannot be dispatched).
- Auto-resolve on state-serial change after an apply (heuristic; evaluate after Phase 4
  data exists).
- **Azure blob state sources still use a shared `account_key`** (`internal/statesource/azure.go:37-47`,
  `azblob.NewSharedKeyCredential`) — the last shared secret in the drift path once Phase 1b
  lands. Follow-up: add an Entra / Workload Identity option to the blob connector
  (`azidentity.NewWorkloadIdentityCredential` → `azblob.NewClient(serviceURL, cred, nil)`),
  reusing the `azidentity` dependency and the `tsm-*` UAMI pattern from Phase 1b; grant the
  identity `Storage Blob Data Reader` (or `Contributor` where TSM writes state) on the
  container. Mirrors the pipelines' `backendAzureRmUseEntraIdForAuthentication`. Keep
  `account_key` as the fallback for non-AKS installs.
