<!-- markdownlint-disable MD013 -->
# 2. Shared Identity Component Owned by Neither App

**Status**: Accepted

## Context

The registry and State Manager backends each independently implement an identity/admin layer — users, organizations, API keys, OIDC config, role templates, and audit logs. The registry's implementation is the mature superset (it also has LDAP, SAML, Azure AD, mTLS, and a JWT-revocation/session `statestore`); the State Manager's is an earlier subset.

The revitalization requires the two products to feel like one suite: a single sign-on experience and **identical** administrative controls. That makes the identity layer the natural — and only — place to share a schema, rather than duplicating it.

A hard constraint was set: **neither application may own the identity space.** Either app must be able to stand identity up at first-run setup, and whichever app is installed *second* must **detect that it already exists and attach** to it rather than recreate it. Both backends are confirmed to run against the **same PostgreSQL database** in every environment.

## Decision

Create a new dedicated repository, **`terraform-suite-identity`**, as a versioned, pinnable Go module owned by neither app. It packages:

- the auth methods (`jwt`, `apikey`, `oidc`, `ldap`, `saml`, `azuread`, `mtls`) and the `statestore` (JWT revocation / sessions),
- the identity data models (`user`, `organization`, `organization_member`, `role_template`, `api_key`, `oidc_config`, `audit_log`, `org_quota`),
- the admin API handlers and RBAC middleware,
- **and its own migration set.**

Identity tables live in a dedicated PostgreSQL **`identity` schema** with their **own** `golang-migrate` version table, separate from each application's migration chain. At setup/startup, each app runs the identity migrations through the shared module:

- the first app creates the `identity` schema and migrates it to version *N*;
- the second app sees the version table, finds a compatible schema already present, and **attaches** (no recreation);
- concurrent first-runs are serialized by PostgreSQL **advisory locks** (golang-migrate is idempotent by version).

Operational requirements for real SSO: both deployments share the **JWT signing secret** and the **`ENCRYPTION_KEY`** (so either can decrypt stored OIDC secrets/tokens) and point at the **same database**. OIDC config is DB-stored, so one configuration drives both apps' login. Identity migrations are **additive / backward-compatible only**; each app pins a minimum identity-module version and asserts the schema is at or above it on startup. The `terraform-suite-identity` repo's CI runs contract/integration tests against **both** consumer apps so an identity change that would break a consumer is caught before release.

### Alternatives considered

- **Shared DB server, separate schemas (each app owns its own identity copy):** simplest isolation, but no cross-app SSO and users/orgs/keys are administered twice — fails the parity goal.
- **Fully independent databases:** maximum isolation, maximum duplication — also fails the goal.
- **Standalone identity *service* both apps call (token introspection):** most literally "owned by neither," but heavier (new deployable, network hop on the hot auth path) and does not fit "either app deploys it at setup." Retained as the fallback **if** the same-database assumption ever breaks in some environment.

## Consequences

**Easier**:

- Genuine single sign-on across the suite and a single, identical administration surface.
- New suite members can adopt identity by importing one versioned module.
- The registry's richer auth (LDAP/SAML/Azure AD/mTLS, revocation) becomes available to the State Manager.

**Harder**:

- Cross-repo release coordination: the two apps must agree on the identity module version, and migrations must stay additive/backward-compatible.
- Both deployments must share secrets (JWT, encryption key) and the same database — a deployment-topology constraint that must hold everywhere.
- The registry backend must be refactored to consume the extracted module (a change to a production app), behind a flag and reversibly.
