<!-- markdownlint-disable MD013 -->
# 1. Suite Coupling via Runtime Discovery and a Shared Identity Schema

**Status**: Accepted

## Context

The State Manager is a standalone product, but it ships alongside a sibling Terraform Registry. An operator may run either one alone or both together. When both run, two integrations become valuable:

1. **Cross-app data joins** — the registry wants a "Consumed by" panel showing which Terraform states reference a given module. That data lives here, in the State Manager.
2. **Single sign-on across both apps** — a user who logged into one app should not be prompted again by the other, and an admin should not maintain two parallel sets of users, organizations, and roles.

Both integrations must be **optional and zero-config in the standalone case**. The State Manager cannot assume the sibling exists, cannot require it to be reachable at boot, and must keep working if the sibling goes away at runtime. It also cannot adopt a heavyweight service mesh or a shared message bus — the deployment story is "two containers plus PostgreSQL" and must stay that simple.

The constraints rule out compile-time coupling (one binary cannot import the other) and rule out a hard boot-time dependency (the sibling may be down when this app starts). What the two apps *can* share is the `github.com/sethbacon/terraform-suite-identity` Go module — the identity primitives (users, organizations, roles, JWT issuance/validation, API keys, audit) — and, optionally, the PostgreSQL **identity schema** those primitives read and write.

## Decision

Couple the two apps through two independent, opt-in mechanisms.

**Runtime manifest discovery.** When `TSM_SUITE_SIBLING_URL` is set, `NewRouter` starts a `suite.DiscoveryClient` (`internal/api/router.go`) that polls the sibling's `/api/v1/suite/manifest` on an interval (`TSM_SUITE_POLL_INTERVAL`, default 60s) and exchanges this app's own manifest (`buildSuiteManifest` in `internal/api/suite.go`). The manifest advertises `App`, `PublicURL`, capabilities (`state.v1`, and `audit.ingest.v1` only under a shared store), and an `Identity` block. The SPA reads the live snapshot via `GET /api/v1/ui/config`; when the sibling is unreachable the snapshot degrades to a state string and the app keeps serving. There is no compile-time and no boot-time dependency — discovery is a background loop.

**Shared identity schema, with one owner.** Identity tables live in their own schema. By default (`identity_database.*` unset) the identity schema is the app database (standalone). To share identity across the suite, an operator points `TSM_IDENTITY_DATABASE_*` at a common database; the connection is opened with `search_path=identity,public` (`config.GetDSNWithSearchPath`) so the shared repositories resolve to the identity schema. Because two apps now write the same `role_templates` and `organizations` rows, **exactly one app must own the role-template seed** — see [ADR 004](004-role-seed-ownership.md).

**Server-to-server reads are gated by a shared service token, never a user session.** `GET /api/v1/consumers` and `POST /api/v1/audit/ingest` sit behind `RequireSuiteServiceToken` (`internal/middleware/suite.go`), which constant-time-compares the `X-Suite-Service-Token` header against `TSM_SUITE_SERVICE_TOKEN`. When the token is empty (the default), the comparison always fails, so the endpoints are effectively *off* until an operator provisions a matching secret on both apps. These routes carry no cookie, so they are outside the CSRF group and cannot be driven by a browser.

**Audit federation is advertised only under a shared store.** `audit.ingest.v1` is added to the manifest only when `TSM_SUITE_IDENTITY_SHARED_STORE` is true, because a sibling's audit entries reference user and org IDs that are coherent only when both apps share the identity schema. Standalone or federated-DB deployments omit the capability, so a sibling never ships entries that would mis-attribute or violate the `user_id` foreign key.

## Consequences

**Easier**:

- The standalone deployment needs zero suite configuration — every coupling knob defaults to off.
- Coupling degrades gracefully: a down or slow sibling never fails this app's requests; the discovery loop simply reports a non-active state.
- SSO and admin de-duplication come "for free" once the identity schema is shared — no token exchange protocol to build, because both apps validate the same issuer's JWTs against the same store.
- The cross-app surface is small and auditable: two service-token endpoints plus a read-only manifest.

**Harder**:

- A shared identity store introduces a cross-app write hazard on the seeded `role_templates` rows, which forces the role-seed-owner election ([ADR 004](004-role-seed-ownership.md)) and the audit-capability gate above.
- The shared service token is a long-lived secret that must be provisioned identically on both apps and rotated in lockstep.
- Operators must understand three distinct modes (standalone, discovery-only, shared-store) and the subtle difference between "sibling reachable" and "sibling shares my identity store" — the SPA's "you may need to sign in" hint is dropped only when *both* apps assert `identity_shared_store`.
