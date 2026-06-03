# Terraform State Manager — Revitalization Plan

> Status: **DRAFT** · Started 2026-06-03 · Spans multiple sessions
> This is a working planning artifact (uncommitted). It is not part of the release workflow yet.

## 1. Vision

Establish **Terraform State Manager** (TSM) as the third pillar of the org's Terraform tooling suite, alongside:

- **terraform-registry** (backend + frontend) — module/provider registry, mirror, scanning, policy.
- **azure-pipelines-terraform** — the ADO extension that installs and runs Terraform with WIF/OIDC.

TSM provides **centralised state analysis, drift health-checks, backup, migration, and compliance** across every Terraform backend, with a UI and auth experience **indistinguishable from the registry**, and an identity layer **shared** with the registry so the suite feels like one product.

## 2. Guiding principles

- **Fact-based, provable.** Every capability ties to a verifiable success criterion (test, e2e, or demonstrable run). No speculative scope.
- **Evolve, don't restart.** The existing backend has real, working services; extend them.
- **Parity by reuse.** The registry is the source of truth for look/feel, i18n, auth, and admin. Re-baseline from it rather than reinvent.
- **Surgical & simple** (per repo CLAUDE.md): minimum code that solves the problem; no premature abstraction. The shared design-system framework is extracted *after* two stable consumers exist, not before.
- **TDD.** Tests/repro-first for new features and fixes; keep the existing quality gate green (`go fmt`/`go vet`/`go test -race`/`gosec`).

## 3. Locked decisions (2026-06-03)

| # | Decision |
|---|----------|
| D1 | **Frontend:** re-baseline state-manager FE from the *current* registry FE (React 19 / MUI 9 / i18next+10 locales / FontAwesome+simple-icons / react-query). Defer shared-framework extraction. |
| D2 | **Identity:** a shared identity/RBAC component **owned by neither app**. Either app stands it up at setup; the second app **detects and attaches**. Source of truth = registry's auth layer. Each app keeps its own feature tables. |
| D3 | **TF execution:** **hybrid** — ADO pipelines (existing extension, `plan -detailed-exitcode`→`changesPresent`) for state-vs-code + version no-op testing; backend read-only cloud queries for state-vs-environment drift. |
| D4 | **Backend:** **evolve** existing services; align auth/admin to the shared identity component. |
| D5 | **First milestone:** Foundation re-baseline (shared identity + FE parity, existing analysis ported) before new features. |
| D6 | **Compliance MVP:** keep existing custom rules (naming/tagging/version) behind a policy abstraction; **add OPA/Rego as fast-follow.** No Sentinel. |
| D7 | **State-vs-env drift clouds (MVP):** HCP Terraform + Azure + AWS + GCP + OCI. Consume HCP native drift (health assessments) where available. |
| D8 | **ADO repo migration (goal 3d):** in MVP scope but sequenced late. Covers git + state + pipeline definitions; **adopts** branch policies + variable groups + service connections in the target org. |
| D9 | **Primary SCM:** Azure DevOps Repos for MVP. |

## 4. Current-state assessment (provable)

### Backend (`terraform-state-manager-backend`) — first draft, 10 migrations
- **Exists & reusable:** `analyzer` (parser/counter/provider/rum/metadata/batch), `snapshot` (snapshot-vs-snapshot diff), `backup` (+retention), `migration` (state-file-between-backends), `compliance` (custom naming/tagging/version rules), `notification`, `reporter`, `scheduler`. State clients for hcp/s3/azure/gcs/consul/pg/k8s/http/local. Repository pattern, JWT/API-key/OIDC auth, RBAC, audit, telemetry.
- **Gaps vs goals:**
  - 3a repo metadata — `analyzer/metadata.go` reads TF version from state + HCP workspace only; no `required_version` / `.terraform.lock.hcl` from a linked repo.
  - 3b drift — only snapshot-vs-snapshot; **no** state-vs-code (plan) or state-vs-environment (cloud).
  - 3d ADO repo migration — `migration.go` is state-file-only; ADO org/repo migration absent.
  - 3e OPA/Sentinel — only custom rules.
  - Auth is a subset of the registry's (no LDAP/SAML/AzureAD/mTLS, no statestore/revocation).

### Frontend (`terraform-state-manager-frontend`) — v0.1
- Seeded from an older registry FE (shares `AuthContext`/`ThemeContext`/`HelpContext`/`Layout`/`ProtectedRoute`/`DevUserSwitcher`/`SetupWizardPage`) and has pages for all major features.
- **Behind on every parity axis:** React 18 vs 19, MUI 6 vs 9; **no i18n**, **no FontAwesome/simple-icons**, **no react-query**, **no command palette / a11y / consent / announcer** infra.

### Reference: registry FE (v2.4.1) & backend (37 migrations)
- FE: the canonical design system (theme, i18n 10 locales, icons, react-query, contexts, command palette, a11y). Has DB-configurable `ui_theme`.
- Backend: canonical identity/admin — `auth/{jwt,apikey,oidc,ldap,saml,azuread,mtls,statestore}`, models `user/organization/organization_member/role_template/api_key/oidc_config/audit_log/org_quota`, full `api/admin` surface (users+GDPR, orgs, apikeys, oidc, rbac, role-templates, audit+export+retention, quotas, stats).

### Existing TF execution: `azure-pipelines-terraform`
- ADO extension: install TF; `init/plan/apply/...` with WIF/OIDC across Azure/AWS/GCP/OCI; backends azurerm/s3/gcs/oci/hcp/generic/local; **`changesPresent`** output on `plan -detailed-exitcode`; module publish. TSM orchestrates this rather than executing Terraform itself.

## 5. Target architecture

### 5.1 Shared Identity Component (the keystone)

**Goal:** one identity/RBAC/admin store, owned by neither app, with detect-and-attach setup and real SSO.

**Realization (confirmed: new dedicated repo + shared database):**
- A **shared Go module in its own repo, `terraform-suite-identity`** (owned by neither app, versioned/pinnable), packaging: auth methods (`jwt/apikey/oidc/ldap/saml/azuread/mtls`), the `statestore` (JWT revocation/sessions), identity models, the admin API handlers, RBAC middleware, **and its own migration set**.
- Both backends run against the **same Postgres database** in every environment (confirmed O2), so the shared-schema model below applies directly.
- Identity tables live in a dedicated Postgres **schema** (`identity`) with their **own golang-migrate version table** (`identity_schema_migrations`), separate from each app's migration chain.
- **Detect-and-attach:** at setup/startup each app runs the identity migrations through the shared module. golang-migrate is idempotent by version and uses Postgres **advisory locks**, so:
  - first app creates the `identity` schema and migrates to version N;
  - second app sees the version table, finds the schema present at a compatible version, and **attaches** (no recreation);
  - simultaneous first-runs are serialised by the advisory lock.
- **SSO requirements (operational):** both deployments must share the **JWT signing secret** (`TSM_AUTH_JWT_SECRET`) and the **`ENCRYPTION_KEY`** (so both decrypt stored OIDC secrets/tokens), and point at the **same Postgres instance/database**. OIDC config is DB-stored → one config drives both apps' login.
- **Compatibility discipline:** identity migrations are **additive/backward-compatible only**; each app pins a minimum identity-module version and asserts schema ≥ required at startup. Releases of the two apps coordinate on the identity module version.
- **Alternative (future, if same-DB assumption breaks):** promote identity to a standalone service both apps call (token introspection). Heavier; only if a shared DB is not viable in some environment.

> Open question O1–O3 (below) must be answered before Phase 1 implementation.

### 5.2 Frontend parity

- Re-baseline TSM FE on a **copy of the current registry FE scaffold**: theme + `ThemeContext`, i18n (`i18next` + locale files for all 10 languages), icon system (FontAwesome + simple-icons), `react-query`, `Layout`, `AuthContext`/`ProtectedRoute`, `CommandPalette`, a11y/consent/announcer, login/callback/setup pages, and the **admin pages** (which talk to the shared identity API → identical by construction).
- Port the existing TSM feature pages (analysis, snapshots, backups, migrations, compliance, reports, alerts, scheduler) onto the new shell, adding i18n keys.
- Defer extracting a shared FE framework until both apps are stable (D1).

### 5.3 Hybrid Terraform execution

- **state-vs-code drift & version no-op testing:** TSM creates/triggers an **ADO pipeline** using `azure-pipelines-terraform` (`plan -out` + `-detailed-exitcode`), then ingests the plan JSON + `changesPresent` via a **webhook callback**. No Terraform or cloud creds in the TSM backend.
- **state-vs-environment drift:** backend **read-only cloud clients** (Azure/AWS/GCP/OCI) reconcile in-state resources against live resources; for **HCP-backed** workspaces, consume HCP **health assessments** (native drift) instead of reimplementing.

### 5.4 Extensibility framework (goal 3f)

Formalize a lightweight **capability contract** (no dynamic plugin system — keep it simple). A capability registers: routes, RBAC scopes, optional scheduled task type(s) (the scheduler already dispatches by type), optional cloud-client needs, and a swagger fragment. New capabilities are additive and discovered via a registration list. Document with one worked example (the version-no-op-test capability in Phase 8).

## 6. Phased roadmap

Each phase has explicit success criteria (goal-driven). Phases 3–6 can parallelize after Phase 2 using the repo's parallel-agent coordination rules; Phase 1 is the keystone; Phase 7 is large and late.

### Phase 0 — Groundwork
- Adopt the **registry workflow** (single `main`; `feat/`/`fix/`/`docs/`/`refactor/` branches; Conventional Commits; squash-merge) and wire its **CI/release automation** into this repo: release-please (+ goreleaser), Conventional-Commit PR-title validation, the `gosec` baseline comparison, and the coverage gate. Confirm the quality gate is green on the existing backend; write ADRs for D1–D4.
- **Done when:** existing backend builds; `go test -race` + `gosec` pass; CI workflows + release-please land on `main`; ADRs committed; open questions O3–O8 answered.

### Phase 1 — Shared Identity Component *(keystone)*
- Extract registry auth/identity into the shared module + `identity` schema + own migration chain + detect-and-attach bootstrap; wire shared JWT/ENCRYPTION secrets + statestore.
- Make **both** registry-backend and TSM-backend consume it (registry change is feature-flagged + reversible).
- **Done when:** a user/api-key/org created via one app authenticates against the other (SSO integration test); second-app setup detects existing identity and attaches (test); migrations idempotent under concurrent start (test); `gosec` clean.

### Phase 2 — Frontend re-baseline + admin parity
- Re-baseline TSM FE on the registry scaffold; port TSM pages; full i18n keys; SSO login via shared identity.
- **Done when:** shared shell + admin screens match the registry (visual/e2e), all TSM strings are i18n-keyed, login/SSO works, lint/test/a11y gates meet the registry's bar.

### Phase 3 — Analysis + repo metadata
- Link state sources ↔ ADO repos; extract `required_version`, `.terraform.lock.hcl` provider constraints, module versions; enrich dashboards with constraint-vs-actual.
- **Done when:** an analysis run on a repo-linked source reports TF/provider constraints vs in-state versions and flags pin drift; tests.

### Phase 4 — Drift health-check (hybrid)
- 4a state-vs-code: schedule ADO pipeline plan per source; ingest plan JSON + `changesPresent` via webhook → drift events.
- 4b state-vs-environment: read-only cloud clients (Azure first, then AWS/GCP/OCI) + consume HCP health assessments.
- **Done when:** a scheduled run produces both code-drift and env-drift events end-to-end on the Azure path; pipeline ingestion works; tests with recorded fixtures.

### Phase 5 — Compliance policy abstraction (+ OPA fast-follow)
- Wrap existing rules behind a policy-engine interface; add an OPA/conftest engine over plan/state JSON.
- **Done when:** the same policy set evaluates via both custom and OPA engines with unified results; tests.

### Phase 6 — Backup/restore + state migration hardening
- Integrity-verified backup→restore round-trip; cross-backend state-file migration dry-run + execute.
- **Done when:** backup→restore verified by checksum; migration dry-run + execute tested across at least two backends.

### Phase 7 — ADO repo migration *(large)*
- ADO REST client; migrate git history + state + pipeline definitions; **adopt** branch policies + variable groups + service connections in the target org; dry-run report + validation + resumability.
- **Done when:** dry-run enumerates everything to move; execute migrates a sample repo incl. pipelines/policies/var-groups; idempotent/resumable; tests against mocked ADO API.

### Phase 8 — Extensibility framework + version no-op testing
- Formalize the capability contract; implement the scheduled "TF/provider version bump no-op test" capability (trigger ADO plan against candidate versions; assert no-op).
- **Done when:** a new capability is addable via the documented contract with a worked example; the version-test capability runs on schedule and reports no-op/drift per repo.

## 7. Cross-cutting (every phase)
- **Workflow (registry-standardized):** issue → branch from `main` → quality gate → PR to `main` (Conventional Commit title) → squash-merge. release-please manages versioning/`CHANGELOG.md`/tags from Conventional Commits; never hand-edit `CHANGELOG.md`.
- **Swagger:** every new/changed handler updates `backend/docs/swagger.yaml` before commit.
- **Security:** `gosec` clean; careful handling of shared JWT/ENCRYPTION secrets and ADO/cloud credentials (prefer WIF/managed identity; no secrets in state-manager where pipelines can hold them).
- **Telemetry & docs** kept current.

## 8. Open questions (next session)

- **O1. [RESOLVED 2026-06-03]** Shared identity lives in a **new dedicated repo** `terraform-suite-identity` (owned by neither app).
- **O2. [RESOLVED 2026-06-03]** Yes — both apps run against the **same Postgres database** in every environment. Shared-schema model applies.
- **O3. [RESOLVED 2026-06-03]** The `terraform-suite-identity` repo **owns compatibility testing**: its CI runs contract/integration tests against both dependent apps (registry + state-manager), so an identity change that would break a consumer is caught in the identity repo before release.
- **O4. [RESOLVED 2026-06-03]** **Multi-tenancy is in MVP** — full multi-org (matching the registry's org IdP binding + quotas). Identity and feature data are org-scoped from the start.
- **O5. [RESOLVED 2026-06-03]** **Initial deployment target = AKS on Azure.** Additional deployment options (ECS, other Helm/overlay targets) are a fast-follow.
- **O6. [DIRECTION 2026-06-03]** ADO auth model still **TBD**; choose the option with **least friction + broadest compatibility** (lean to WIF/OIDC service connection where supported, PAT/service-principal only where required). Finalize before the ADO-touching phases (4a, 7, 8).
- **O7. [RESOLVED 2026-06-03]** State source ↔ ADO repo linkage is **auto-discovered, with manual override/fallback**.
- **O8. [RESOLVED 2026-06-03]** **Assume HCP Plus / health assessments are available**, with a **fallback to pipeline-plan drift** when they are not.

## 9. Recommended next step

O1–O2 are resolved. Open the Phase 0 issue, adopt the registry workflow + CI/release automation on `main`, and write the D1–D4 ADRs. Phase 1 (shared identity) is the keystone everything else depends on.
