<!-- markdownlint-disable MD013 MD060 -->
# Threat Model — Terraform State Manager

**Document version:** 1.0
**Last updated:** 2026-06-17
**Methodology:** STRIDE (Spoofing, Tampering, Repudiation, Information Disclosure, Denial of Service, Elevation of Privilege)

---

## 1. System Overview

The Terraform State Manager is a self-hosted control plane that connects to Terraform state where it already lives, analyzes it, edits it under a safety pipeline, and watches it for drift. It comprises:

- **Backend API server** (Go) — handles authentication, authorization, state-source connectors, the edit pipeline, drift, Version Lab, scheduling, and the suite-coupling surface.
- **Frontend SPA** (React) — admin UI served by nginx.
- **PostgreSQL** — app schema (sources, analysis, backups, locks, drift, schedules) and the shared identity schema (users, orgs, roles, tokens, audit).
- **State sources** (external) — HCP/TFC, S3, Azure Blob, GCS, Consul, PostgreSQL, Kubernetes, HTTP, Git, local filesystem. Connected outward, on demand.
- **CI systems** (external) — GitHub Actions / Azure DevOps, dispatched for drift and Version Lab runs, posting results back via per-run callback tokens.

Unlike a registry, TSM does not warehouse the artifacts it manages: state is read on demand and only **analysis records** and **pre-write backups** are persisted.

## 2. Data Flow Diagram

```text
┌───────────────────────────────────────────────────────────────────┐
│                       External Zone                               │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────────────┐  │
│  │ Browser  │  │  CI/CD   │  │ Sibling  │  │  IdP (OIDC/SAML/ │  │
│  │  User    │  │ runners  │  │ Registry │  │       LDAP)      │  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────────┬─────────┘  │
└───────┼─────────────┼─────────────┼─────────────────┼────────────┘
        │             │             │                 │
   ─────┼─────────────┼─────────────┼─────────────────┼──── TLS boundary
        ▼             ▼             ▼                 ▼
┌───────────────────────────────────────────────────────────────────┐
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                    Load Balancer / Ingress                  │  │
│  └──────────────────────────┬──────────────────────────────────┘  │
│         ┌───────────────────┼────────────────────┐                │
│         ▼                                        ▼                │
│  ┌──────────────┐                        ┌──────────────┐         │
│  │   Frontend   │───── /api/* proxy ────▶│   Backend    │         │
│  │   (nginx)    │                        │   (Go API)   │         │
│  └──────────────┘                        └──────┬───────┘         │
│                                                 │                 │
│           ┌──────────────────┬──────────────────┤                 │
│           ▼                  ▼                  ▼                  │
│    ┌───────────┐    ┌────────────────┐   ┌──────────────┐         │
│    │PostgreSQL │    │  State sources │   │  CI systems  │         │
│    │ app +     │    │  (10 backends, │   │  (GH / ADO,  │         │
│    │ identity  │    │  outbound)     │   │  outbound)   │         │
│    └───────────┘    └────────────────┘   └──────────────┘         │
│                       Internal Zone                               │
└───────────────────────────────────────────────────────────────────┘
```

## 3. Trust Boundaries

| Boundary | Description |
| -------- | ----------- |
| **TB-1** | Internet → Load Balancer: TLS terminates here. All external traffic encrypted in transit. |
| **TB-2** | Load Balancer → Backend/Frontend: internal network; may use mTLS in zero-trust environments. The backend can also terminate TLS itself for the direct mTLS path. |
| **TB-3** | Backend → PostgreSQL (app + identity schema): credentials-authenticated, ideally TLS (`ssl_mode=require`+). |
| **TB-4** | Backend → State sources: each connector authenticates with credentials decrypted from the database at request time; TLS to the backend. |
| **TB-5** | Backend → IdP: OIDC/SAML/LDAP over TLS. |
| **TB-6** | Backend ↔ CI runners: outbound dispatch over TLS; inbound results authenticated by a per-run one-shot callback token. |
| **TB-7** | Backend ↔ Sibling app: server-to-server cross-app reads gated by a shared `X-Suite-Service-Token`. |

## 4. Assets

| Asset | Sensitivity | Description |
| ----- | ----------- | ----------- |
| Terraform state (read on demand) | Critical | Contains resource attributes and frequently embedded secrets |
| State-source credentials | Critical | HCP tokens, cloud keys, Consul/PG/K8s creds — AES-256-GCM encrypted at rest |
| CI credentials | Critical | GitHub/ADO tokens for dispatch — encrypted at rest |
| Notification target URLs | High | Slack/webhook destinations — secret, encrypted at rest |
| API keys (hashed) | Critical | bcrypt-hashed; compromise enables scoped access |
| `TSM_JWT_SECRET` | Critical | Signs session JWTs |
| `TSM_ENCRYPTION_KEY` | Critical | Decrypts every stored credential; loss orphans them |
| Suite service token | High | Shared secret authorizing cross-app reads |
| Setup token (hashed) | High | bcrypt-hashed first-run bootstrap credential |
| Audit logs | High | Compliance evidence of who edited which state |
| Pre-write state backups | High | Full copies of prior state, same sensitivity as live state; unlike the credential/secret rows above, stored **unencrypted** at rest — access-controlled only (see I-8) |
| User PII | Medium | Emails, usernames, IdP identifiers |

## 5. STRIDE Analysis

### 5.1 Spoofing (S)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| S-1 | Attacker uses a stolen API key | Backend auth | API keys bcrypt-hashed (never plaintext); indexed-prefix lookup + bcrypt compare; optional expiry enforced at auth time; scopes capped at creator's own | ✅ Implemented |
| S-2 | Attacker forges OIDC/SAML assertions | Backend auth | Token signature verified against IdP metadata/JWKS; SAML XSW round-trip + InResponseTo binding; IdP-initiated SSO off by default; OIDC state/nonce | ✅ Implemented |
| S-3 | LDAP injection bypasses auth | Backend LDAP | Username escaped before filter substitution; empty passwords rejected (no unauthenticated bind); LDAPS/StartTLS in production | ✅ Implemented |
| S-4 | Session hijack via XSS | Frontend / Backend | Session JWT in HttpOnly `tsm_auth_token` cookie (unreadable by JS); strict API CSP `default-src 'none'`; `Secure` flag derived from public URL scheme | ✅ Implemented |
| S-5 | Forged cross-app request reads consumer data | Suite endpoints | `GET /consumers` and `POST /audit/ingest` require `X-Suite-Service-Token`, constant-time compared; disabled when the token is empty (default) | ✅ Implemented |
| S-6 | Forged CI callback posts fake drift/health results | Drift / Version Lab | Result callbacks authenticated by a per-run one-shot token, not a user session | ✅ Implemented |

### 5.2 Tampering (T)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| T-1 | Concurrent edits corrupt a state file | Edit pipeline | Single-writer locking: native (HCP lock-then-verify, Consul check-and-set, local lock file) or app-level advisory lock with a 15-min stale TTL ([ADR 003](adr/003-advisory-lock-ttl.md)) | ✅ Implemented |
| T-2 | A write proceeds blind during a backend outage | Edit pipeline | Fail-closed: a write aborts (`502`) unless the current state is positively verified or definitively absent ([ADR 002](adr/002-fail-closed-state-writes.md)) | ✅ Implemented |
| T-3 | A stale write regresses a state advanced elsewhere | Edit pipeline | Serial non-regression + lineage match enforced unless `force=true`; the override is audited | ✅ Implemented |
| T-4 | SQL injection modifies records | Backend | Parameterized queries throughout (`database/sql`/`sqlx`); no string-interpolated SQL | ✅ Implemented |
| T-5 | An unrecoverable edit destroys state | Edit pipeline | Current state is backed up before every write; restore endpoint replays a prior backup through the same guarded pipeline | ✅ Implemented |
| T-6 | Path traversal via the setup-token file path | Startup | `SETUP_TOKEN_FILE` rejects `..` sequences and is `filepath.Clean`'d before write | ✅ Implemented |
| T-7 | Supply-chain tampering of the container image | Build/Deploy | Pinned base image digest; non-root user (uid 1000); minimal Alpine runtime; release signing/SBOM via suite release tooling | ✅ Implemented |

### 5.3 Repudiation (R)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| R-1 | A user denies editing a state | Audit system | Every mutating state action (`state.edit`, `state.force_unlock`, transfers) logged with actor, resource, and metadata | ✅ Implemented |
| R-2 | An admin denies changing roles/sources | Audit system | Admin mutations recorded in the shared identity audit log | ✅ Implemented |
| R-3 | Force-unlock or forced override hides an action | Edit pipeline | `force` overrides and force-unlock are explicit, audited actions with the released/overridden detail recorded | ✅ Implemented |

### 5.4 Information Disclosure (I)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| I-1 | Stored source credentials exposed via the API | Sources API | Credentials AES-256-GCM encrypted at rest and never serialized back to clients | ✅ Implemented |
| I-2 | Encryption/JWT secrets leak via config | Backend | Both secrets read only from the environment, never the YAML/Helm values; separate keys for signing vs. encryption | ✅ Implemented |
| I-3 | Unauthorized read of another team's state | State API | Scope-based RBAC (`state:read`) on every read endpoint | ✅ Implemented |
| I-4 | Notification target URLs leaked to non-admins | Notifications | The entire notification-channels group is `admin`-scoped because target URLs are secrets | ✅ Implemented |
| I-5 | `/metrics` exposes internal topology | Telemetry | Prometheus served on a separate internal port (`:9090`), off the public ingress; must be firewalled to the monitoring network | ⚙️ Operator-configured |
| I-6 | Setup token printed to logs after use | Startup | Only the bcrypt hash is persisted; the raw token is single-use and invalidated on completion; an operator-supplied token is never echoed | ✅ Implemented |
| I-7 | Secrets embedded in state surfaced in analysis | Analyzer | Analysis records counts/metadata, not attribute values; raw-state read is `state:read`-gated; backups inherit the same access controls | ✅ Implemented / ⚙️ |
| I-8 | `state_backups.data` (or a raw DB backup/dump) exposes full plaintext state, including any embedded secrets | PostgreSQL / Edit pipeline | Access limited to DB credentials and the `state:read`-scoped API; unlike source/CI credentials, notification URLs, and API keys, backup contents are **not encrypted at rest** and are **not automatically pruned** (see [capacity-planning.md](capacity-planning.md#database-sizing)). Encryption at rest and a retention policy are planned — tracked in #257 | ⚠️ Partial |

### 5.5 Denial of Service (D)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| D-1 | A crashed editor wedges a state key forever | Locking | App-level locks carry a 15-min stale TTL reaped on the next acquire; admin force-unlock for sooner recovery ([ADR 003](adr/003-advisory-lock-ttl.md)) | ✅ Implemented |
| D-2 | Oversized request body exhausts memory | Edit API | State writes are read through a `LimitReader` (`maxUploadBytes`) | ✅ Implemented |
| D-3 | Database connection exhaustion | Backend | Bounded connection pool (`max_connections` per replica); per-operation query timeouts | ✅ Implemented |
| D-4 | A slow/hung state source stalls the sync loop | State-sync | Per-source reconcile timeout (10 min); only one reconcile cycle runs at a time | ✅ Implemented |
| D-5 | Two worker replicas double-fire schedules | Scheduler | `TSM_WORKERS_ENABLED` gate; periodic loops run on exactly one dedicated replica; overdue schedules fire once then reschedule (no catch-up storm) | ⚙️ Operator-configured |
| D-6 | API flooding | Ingress | Rate limiting applied at the proxy/ingress; auth-heavy endpoints (LDAP login, SCIM) documented to sit behind it | ⚙️ Operator-configured |

### 5.6 Elevation of Privilege (E)

| ID | Threat | Component | Mitigation | Status |
| --- | --- | --- | --- | --- |
| E-1 | A read-only user escalates to write/admin | Backend RBAC | Scope checked on every endpoint via `RequireScope`; scopes loaded server-side at request time, not trusted from the client | ✅ Implemented |
| E-2 | An API key grants more than its creator holds | API keys | Key scopes are capped at the creator's own scopes; ownership enforced in the handler | ✅ Implemented |
| E-3 | A SCIM token reaches non-provisioning endpoints | SCIM | SCIM routes mounted only when enabled, gated by the dedicated `scim:provision` scope | ✅ Implemented |
| E-4 | A spoofed IdP group claim grants admin | SSO group mapping | Group→role mappings are admin-configured, never user-supplied; applied only from the cryptographically verified token/assertion; reconciled (and revoked) on every login | ✅ Implemented |
| E-5 | The sibling app clobbers role scopes in a shared store | Suite identity | A single role-seed owner is elected (`role_seed_owner`); the non-owner skips role seeding ([ADR 004](adr/004-role-seed-ownership.md)) | ✅ Implemented |
| E-6 | Spoofed `X-Forwarded-*` headers from an untrusted client | Proxy trust | `TrustedProxies` defaults to trusting no proxy; only configured CIDRs may set forwarded headers | ✅ Implemented |
| E-7 | Container escape to the host | Deployment | Non-root container user; minimal runtime image; seccomp/AppArmor and read-only rootfs recommended in deployment docs | ⚙️ Operator-configured |

> **Status legend:** ✅ Implemented = enforced by the application; ⚠️ Partial = partially enforced; ⚙️ Operator-configured = the application ships safe defaults and guidance, but full enforcement depends on deployment configuration.

## 6. Assumptions

1. **TLS termination** is handled by the load balancer or ingress; the backend may serve TLS directly (required for the direct mTLS path).
2. **PostgreSQL** is on a private network, not internet-reachable, and ideally requires TLS (`ssl_mode=require` or stricter).
3. **State-source credentials** stored in TSM are the least-privilege credentials needed for read (and, where used, write) of the targeted state — not broad cloud-admin keys.
4. **`TSM_ENCRYPTION_KEY` and `TSM_JWT_SECRET`** are injected from a secrets manager, never committed, and the encryption key is escrowed.
5. **Container orchestration** provides network segmentation, and `/metrics` (`:9090`) is firewalled to the monitoring network.
6. **The single dedicated worker replica** is configured correctly — exactly one replica with `TSM_WORKERS_ENABLED=true`.

## 7. Residual Risks

| ID | Risk | Likelihood | Impact | Mitigation Plan |
| --- | --- | --- | --- | --- |
| RR-1 | Zero-day in Go stdlib or a dependency | Low | High | Dependabot + OSV scanning; SBOM enables rapid impact assessment |
| RR-2 | An admin with full scope edits state maliciously | Medium | High | Audit logging; pre-write backups enable forensic comparison and revert; require IdP-enforced MFA for admins |
| RR-3 | State written directly by `terraform apply` (bypassing TSM) races a TSM edit on an unlocked backend | Medium | Medium | Serial/lineage guard rejects stale writes; native locking where the backend supports it |
| RR-4 | Compromised IdP pushes malicious group claims | Low | High | Group→scope mapping limits blast radius; reconciliation revokes on group loss; audit logging detects anomalies |
| RR-5 | Loss of `TSM_ENCRYPTION_KEY` orphans all stored credentials | Low | High | Key custody/escrow documented in disaster-recovery; rotation procedure in secrets-rotation |
| RR-6 | `state_backups.data` is unencrypted at rest and grows without an automatic retention/pruning policy | Low | High | Encryption at rest and a retention/pruning policy are planned, tracked in #257; today, rely on DB access control, network isolation (assumption 2), and the `state:read` API gate |

## 8. Review Schedule

This threat model should be reviewed:

- On every major version release (x.0.0)
- When a new state-source connector or authentication mechanism is added
- When the suite-coupling surface or identity-sharing model changes
- At least annually, even if no significant changes occurred

## 9. References

- [Architecture](architecture.md) — system architecture documentation
- [ADR-001: Suite coupling & shared identity](adr/001-suite-coupling-shared-identity.md)
- [ADR-002: Fail-closed state writes](adr/002-fail-closed-state-writes.md)
- [ADR-003: Advisory lock TTL](adr/003-advisory-lock-ttl.md)
- [ADR-004: Role-seed ownership](adr/004-role-seed-ownership.md)
- [OWASP STRIDE](https://owasp.org/www-community/Threat_Modeling) — methodology reference
