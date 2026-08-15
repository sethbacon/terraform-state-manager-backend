<!-- markdownlint-disable MD013 -->
# Suite Coupling

The Terraform State Manager (TSM) runs **standalone by default**. When it is
deployed alongside its sibling, the [Terraform Registry](https://github.com/sethbacon/terraform-registry-backend),
the two apps can be **coupled at runtime** so they discover each other, share one
identity store, and surface cross-app data ("Consumed by", module freshness, a
unified audit trail). Coupling is entirely operator-driven configuration — there
is no build flag and no code change on either side.

This guide covers the coupling model, the `TSM_SUITE_*` and
`TSM_IDENTITY_DATABASE_*` configuration, the cross-app endpoints
(`/api/v1/suite/manifest`, `/api/v1/consumers`, `/api/v1/audit/ingest`) and their
auth model, the audit-federation receiving side, and the canonical-host matching
that powers the "Consumed by" join.

## Table of Contents

1. [Standalone vs Coupled](#standalone-vs-coupled)
2. [Configuration](#configuration)
3. [Runtime Discovery — the manifest](#runtime-discovery--the-manifest)
4. [Shared identity store](#shared-identity-store)
5. [Cross-app endpoints](#cross-app-endpoints)
6. [Audit federation (receiving side)](#audit-federation-receiving-side)
7. ["Consumed by" and canonical-host matching](#consumed-by-and-canonical-host-matching)
8. [Module freshness (reverse direction)](#module-freshness-reverse-direction)
9. [Coupling checklist](#coupling-checklist)

---

## Standalone vs Coupled

| Aspect | Standalone (default) | Coupled |
| --- | --- | --- |
| Identity store | TSM's own app database (`identity` schema) | One **shared** identity database with the registry |
| Sibling discovery | none | polls the registry's manifest every `poll_interval` |
| "Consumed by" (registry → TSM) | inert (endpoint disabled) | registry server-proxies `GET /consumers` |
| Module freshness (TSM → registry) | every module reports `no_registry` | live comparison against the registry's latest |
| Audit trail | TSM-local only | registry can federate its audit entries into TSM |
| Role-template seeding | TSM seeds its own store | exactly one app seeds the shared store |

Coupling is **additive and graceful**: every cross-app feature degrades to its
standalone behaviour when the sibling is absent, unreachable, or incompatible.
The freshness page returns HTTP 200 with `no_registry` verdicts, the discovery
client reports `degraded`/`unreachable` without erroring requests, and the
service-token-gated endpoints simply stay disabled until a token is provisioned
on both apps.

> Coupling is not all-or-nothing. You can share the identity store without
> enabling audit federation, or enable the "Consumed by" join without a shared
> store. Each feature has its own precondition (below).

---

## Configuration

All coupling configuration is under the `suite` section (env prefix `TSM_SUITE_`)
plus the optional `identity_database` section (`TSM_IDENTITY_DATABASE_`). As
everywhere in TSM, layering is built-in defaults → optional YAML file
(`CONFIG_PATH`) → `TSM_`-prefixed environment variables, which always win. See
[configuration.md](configuration.md) for the full variable reference.

### `TSM_SUITE_*`

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_SUITE_SIBLING_URL` | (empty ⇒ standalone) | Base URL of the sibling registry, e.g. `https://registry.example.com`. Set it to start the discovery poll loop. |
| `TSM_SUITE_POLL_INTERVAL` | `60s` | How often TSM polls the sibling's manifest. |
| `TSM_SUITE_IDENTITY_SHARED_STORE` | `false` | Operator assertion that **this app uses the shared identity store + single IdP**. Advertised in the manifest; gates audit-federation ingest and drops the SPA's "you may need to sign in" hint (only when both apps assert it). |
| `TSM_SUITE_SERVICE_TOKEN` | (empty ⇒ disabled) | Shared secret a sibling presents (`X-Suite-Service-Token`) for server-to-server reads (`GET /consumers`, `POST /audit/ingest`). Set it to the **same value** as the registry's `TFR_SUITE_SIBLING_TOKEN`. |
| `TSM_SUITE_ROLE_SEED_OWNER` | `self` | Who seeds the **shared** identity schema's system role templates: `self` \| `registry` \| `tsm`. With a shared store, exactly one app must own that seed. It no longer decides what TSM's own roles grant — TSM seeds and reads its own copy. |

> **The token is symmetric, the URLs are not.** Each app points its
> `*_SIBLING_URL` at the *other* app. The service token, by contrast, is one
> shared secret that must match byte-for-byte on both sides. TSM compares it in
> constant time.

### `TSM_IDENTITY_DATABASE_*`

By default TSM stores its identity data (users, organizations, roles, tokens,
OIDC config, audit logs — the `identity` schema) **in its own application
database**. To share one identity store across the suite, point this section at
the shared database. Any field left unset falls back to the corresponding
`TSM_DATABASE_*` value, so you typically override only `HOST` and `NAME`.

| Variable | Falls back to | Description |
| --- | --- | --- |
| `TSM_IDENTITY_DATABASE_HOST` | `TSM_DATABASE_HOST` | Identity DB host |
| `TSM_IDENTITY_DATABASE_PORT` | `TSM_DATABASE_PORT` | |
| `TSM_IDENTITY_DATABASE_NAME` | `TSM_DATABASE_NAME` | Identity DB name |
| `TSM_IDENTITY_DATABASE_USER` | `TSM_DATABASE_USER` | |
| `TSM_IDENTITY_DATABASE_PASSWORD` | `TSM_DATABASE_PASSWORD` | **secret** |
| `TSM_IDENTITY_DATABASE_SSL_MODE` | `TSM_DATABASE_SSL_MODE` | |
| `TSM_IDENTITY_DATABASE_MAX_CONNECTIONS` | `TSM_DATABASE_MAX_CONNECTIONS` | |
| `TSM_IDENTITY_DATABASE_MIN_IDLE_CONNECTIONS` | `TSM_DATABASE_MIN_IDLE_CONNECTIONS` | |

The identity connection uses a per-connection `search_path` so the shared
identity repositories — which query unqualified table names — resolve to the
`identity` schema. For a true shared store, **both apps must point at the same
physical database (one host + one database name)**, not merely two databases with
the same schema.

```yaml
# config.yaml — coupled, shared identity store
identity_database:
  host: identity-db.example.com
  name: terraform_suite_identity
  # port/user/password/ssl_mode inherited from `database` unless set

suite:
  sibling_url: https://registry.example.com
  identity_shared_store: true
  service_token: "${TSM_SUITE_SERVICE_TOKEN}"   # equal to registry TFR_SUITE_SIBLING_TOKEN
  role_seed_owner: tsm                           # one app owns the shared seed
```

---

## Runtime Discovery — the manifest

Every Suite app publishes a self-describing **capability manifest** at:

```http
GET /api/v1/suite/manifest
```

This endpoint is **unauthenticated and read-only** (it advertises only public
coupling metadata) and is cached for 30 seconds (`Cache-Control: public, max-age=30`).
TSM's manifest looks like:

```json
{
  "schemaVersion": "suite/v1",
  "app": "terraform-state-manager",
  "version": "1.4.0",
  "buildDate": "2026-06-15T12:00:00Z",
  "publicUrl": "https://tsm.example.com",
  "identity": {
    "issuer": "terraform-state-manager",
    "sharedStore": true,
    "schema": "identity"
  },
  "capabilities": [
    { "id": "state.v1" },
    { "id": "audit.ingest.v1" }
  ],
  "links": { "sourceDetail": "/sources/{id}" }
}
```

Notable rules:

- **`publicUrl`** is `server.public_url` (falling back to `base_url`). It is the
  browser-facing URL the SPA uses for cross-app deep links.
- **`audit.ingest.v1`** is advertised **only when `identity_shared_store: true`**.
  It tells a sibling it may federate its audit trail here. Standalone /
  federated-DB mode omits it so a sibling never ships entries that would
  mis-attribute or fail the `user_id` foreign key. `state.v1` is always present.
- The schema is **additive**: never remove or repurpose a field; consumers
  ignore unknown fields. Two siblings are compatible only when the schema MAJOR
  token matches (`suite/v1` → `v1`).

### The discovery client

When `TSM_SUITE_SIBLING_URL` is set, TSM starts a background poll loop against
the sibling's `/api/v1/suite/manifest`. It tracks one of four states:

| State | Meaning |
| --- | --- |
| `unknown` | not yet polled |
| `active` | reachable **and** compatible (schema major matches, sibling app id differs from self) |
| `degraded` | a poll failed, but within the 5-minute grace window of a prior success — last-good manifest is still served |
| `unreachable` | unreachable beyond grace, or incompatible |

Each poll has a 2-second timeout, so a slow or down registry never blocks a TSM
request. `NegotiateCompat` rejects a sibling whose `app` id is empty or equals
TSM's own (a "pointing at yourself" misconfiguration) — set each app's
`*_SIBLING_URL` to the *other* app.

The SPA reads the live coupling state from `GET /api/v1/ui/config`, which
forwards the sibling's `app`, `state`, `publicUrl`, `links`, and `issuer`. The
seamless-SSO hint is dropped only when **both** apps assert `sharedStore` (TSM's
own `identity_shared_store` **and** the sibling manifest's `identity.sharedStore`).

---

## Shared identity store

When `identity_database` points two apps at one physical database, both apps read
and write the same `identity` schema (users, organizations, roles, tokens, OIDC
config, audit logs). This is what makes a single sign-on and a unified audit
trail coherent: the registry's `user_id` / `organization_id` resolve in TSM's
identity tables, and vice versa.

### Role-template seeding ownership

TSM owns a set of role templates (`admin`, `editor`, `operator`, `viewer`, …)
that it **upserts on every startup**. It now writes them **twice**, and the two
writes have different rules:

- Into **TSM's own** `role_templates`, on the application connection,
  **unconditionally**. `name` is unique *per application* there, so there is no
  sibling to collide with. These are the rows TSM authorizes from — see
  [Authorization source](configuration.md#authorization-source).
- Into the **shared** `identity.role_templates`, gated by
  `TSM_SUITE_ROLE_SEED_OWNER`. `identity.role_templates.name` is globally unique,
  so with a shared store two apps seeding it would still overwrite each other's
  mappings on every restart. That copy is now read by the **sibling registry**
  and by TSM's authorization **rollback path**, not by TSM's own authorization.

`TSM_SUITE_ROLE_SEED_OWNER` therefore still matters, with a narrower job:

- `self` (default, standalone): every app seeds the shared store too.
- `registry` or `tsm`: only the named owner seeds it; the other app skips it.

> **Coupled deployments: your roles changed meaning.** Before the reads moved,
> TSM authorized from the shared table — so with `role_seed_owner=registry`, TSM's
> `editor` granted whatever the *registry* defined. It now grants what TSM
> defines. Run `tsm-server authz-drift` on the current build before upgrading:
> its `template_drift` output names exactly which roles change and how.

The single-tenant **default organization** is always ensured regardless of seed
ownership — it is a per-app convenience, not a cross-app collision.

> Pick exactly one seed owner when sharing a store. If you let both apps seed,
> the role scopes you see depend on whichever app restarted last.

---

## Cross-app endpoints

| Endpoint | Auth | Direction | Enabled when |
| --- | --- | --- | --- |
| `GET /api/v1/suite/manifest` | none (public) | sibling → TSM | always |
| `GET /api/v1/ui/config` | none (SPA) | SPA → TSM | always (sibling block populated only when coupled) |
| `GET /api/v1/consumers` | `X-Suite-Service-Token` | registry → TSM | `service_token` set |
| `POST /api/v1/audit/ingest` | `X-Suite-Service-Token` | registry → TSM | `service_token` set **and** `identity_shared_store: true` |

### Service-token auth

`GET /consumers` and `POST /audit/ingest` are **server-to-server** calls from the
sibling that carry no user session. They are gated by `RequireSuiteServiceToken`,
which compares the request's `X-Suite-Service-Token` header against
`TSM_SUITE_SERVICE_TOKEN` in **constant time**. When the configured token is
empty (the default), the endpoint is effectively disabled — every request is
rejected with `401` — so it stays off until an operator provisions a matching
token on both apps. These endpoints are outside the cookie-CSRF group because
they are not cookie-authenticated.

---

## Audit federation (receiving side)

The registry already writes every audited action to its own database and can
*ship* a copy of each entry to external destinations via its built-in audit
**webhook shipper**. Federation is just a webhook shipper whose destination is
TSM's ingest endpoint:

```text
registry audit middleware ──ship──▶ POST {tsm}/api/v1/audit/ingest ──▶ identity.audit_logs
```

TSM records the entry in the shared `identity.audit_logs` table — the same trail
its **Administration → Audit logs** page reads — tagging it `federated: true`,
along with `source_app` (from the optional `X-Suite-Source-App` header) and the
original `source_timestamp`, `auth_method`, and `status_code` folded into
metadata (those last three have no dedicated `audit_logs` columns).

### Preconditions enforced by TSM

The `POST /audit/ingest` handler enforces both:

1. **Shared identity store.** The handler **refuses with `403`** unless
   `identity_shared_store: true`. This matches the `audit.ingest.v1` manifest
   capability, which TSM advertises only under a shared store. Without it, the
   sibling's actor IDs would not resolve here and the `audit_logs` foreign key
   would reject the row.
2. **Valid service token.** As with `/consumers`, a matching
   `X-Suite-Service-Token` is required.

The wire shape mirrors the registry's internal audit `LogEntry` field-for-field,
so the registry's existing shipper federates with no code change. The handler is
resilient: if a row's `user_id`/`organization_id` does not resolve here (e.g.
`sharedStore` was mis-declared, or the actor was provisioned only in the
sibling), TSM degrades to an attributed-in-metadata record — it nulls the actor
foreign keys, preserves the originals under `federated_user_id` /
`federated_organization_id` in metadata, and retries — rather than 500-storming
the shipper. Delivery is best-effort and asynchronous on the registry side.

> For the registry-side `config.yaml` shipper block (a structured
> `audit.shippers` list, configured by file because it cannot be expressed as
> environment variables), see the registry's `docs/suite-audit-federation.md`.

---

## "Consumed by" and canonical-host matching

The registry's module detail page shows a **"Consumed by"** panel: which states,
across your fleet, call this module. The data lives in TSM. The registry
**server-proxies** to TSM's read surface:

```http
GET /api/v1/consumers?host=registry.example.com&module=namespace/name/system
X-Suite-Service-Token: <shared token>
```

### Where the provenance comes from

When a full `terraform show -json` plan is pushed to `POST /api/v1/drift/ingest`,
TSM captures each registry module the state calls — its source address
(`module_source`, e.g. `terraform-aws-modules/vpc/aws`), the host the module is
served from (`registry_host`, e.g. `registry.terraform.io`), and the locked
version when a lockfile contract makes it available — into the
`state_module_refs` table (migration `000015`). Capture is **best-effort**:
sources that post only pre-computed drift counts have no provenance, so the UI
treats missing provenance as normal, not an error. The set is replaced wholesale
per `(source, state)` on each ingest.

`module_source` and `registry_host` are **plain strings** — the cross-app join
key, with no foreign key into any registry. The join is matched on
**`(registry_host, module_source)`**, so a *local* module named like a public one
never produces a false "consumed by" result.

### Why canonicalization is needed

The host captured from a Terraform module source address, the registry's
service-discovery host, and the registry's own public host can legitimately
differ only in:

- **case** (`Registry.Example.com`),
- a **default port** (`:80` / `:443`),
- a **trailing FQDN dot** (`registry.example.com.`),
- an accidental **scheme prefix** (`https://registry.example.com`), or
- **Unicode (IDN) vs punycode** encoding.

A naive exact-match join would miss rows that differ only cosmetically. The fix
has two layers:

1. **Application layer (`suite.CanonicalHost`).** Both the capture path and the
   `/consumers` read path canonicalize the host: strip any scheme, lowercase,
   drop a trailing dot, fold an internationalized host to punycode ASCII
   (best-effort — a host the IDNA lookup profile rejects, e.g. one with
   underscores, is left lowercased), and drop a default port while preserving a
   non-default one.
2. **Engine layer (a generated column).** Migration `000016` adds a `STORED`
   generated column `registry_host_canon` to `state_module_refs`:

   ```sql
   registry_host_canon TEXT GENERATED ALWAYS AS (
     regexp_replace(lower(regexp_replace(registry_host, ':(80|443)$', '')), '[.]$', '')
   ) STORED
   ```

   Postgres derives this for every row at migrate time and on every future write,
   so the join also rescues **legacy rows captured before canonicalization** with
   no backfill job and no ingest pause. The cross-app index is repointed at
   `(registry_host_canon, module_source)`. The raw `registry_host` is preserved
   unchanged as the audit/provenance value. (IDN/punycode folding is not
   expressible in pure SQL, so non-ASCII *legacy* hosts are only case/port/dot
   folded by the column; new rows are punycode-folded by the Go canonicalizer.)

### Host aliases

A registry may emit several acceptable host identities — its public host, its
discovery host, plus operator-configured aliases — as **repeated `?host=`**
query parameters. TSM canonicalizes and de-duplicates them, then matches a row if
its canonical host is **any** of them (`registry_host_canon = ANY(...)`). This
tolerates vanity-CNAME and port-asymmetry deployments. A single `?host=` (the
pre-alias contract) still works.

Both `host` and `module` are required; an empty result is a normal `200` with an
empty `consumers` array, not an error.

---

## Module freshness (reverse direction)

The "Consumed by" join runs registry → TSM. The **reverse** direction is module
freshness: for each registry module captured in a state, TSM asks the sibling
registry what the latest published version is and reports whether the locked
version is behind.

```http
GET /api/v1/sources/{id}/modules/freshness
```

This is a **TSM → registry** call against the registry's **public** versions
endpoint (`GET /v1/modules/{namespace}/{name}/{system}/versions`) — it is
**unauthenticated** (no suite service token), the opposite of the `/consumers`
proxy. Each per-module call has a 2-second budget so a slow registry can't block
the page. The verdict per module is one of `up_to_date`, `behind`,
`constraint_only`, `no_registry`, or `unknown`.

It is **inert when standalone**: with no active sibling, every module reports
`no_registry` and the response is always `200`, so the page never breaks when the
registry is absent.

---

## Coupling checklist

To enable a fully coupled deployment:

- [ ] Both apps point at **one physical identity database** (`TSM_IDENTITY_DATABASE_HOST` + `_NAME` on TSM; equivalent on the registry).
- [ ] Set `TSM_SUITE_IDENTITY_SHARED_STORE=true` on TSM (and the registry's equivalent).
- [ ] Choose exactly one **shared-schema** role-seed owner (`TSM_SUITE_ROLE_SEED_OWNER` = `tsm` or `registry`, the same decision on both apps).
- [ ] Run `tsm-server authz-drift` on the deployment **before** upgrading it onto a build that authorizes from TSM's own tables, and require a zero exit. Its `template_drift` output names the roles whose meaning changes.
- [ ] Set `TSM_SUITE_SIBLING_URL` on TSM to the registry's public URL (and the registry's `*_SIBLING_URL` to TSM's).
- [ ] Provision one shared `TSM_SUITE_SERVICE_TOKEN` equal to the registry's `TFR_SUITE_SIBLING_TOKEN`.
- [ ] Confirm pod-to-pod reachability both ways (manifest polls, `/consumers`, freshness, audit shipping).
- [ ] (Optional) Configure the registry's `audit.shippers` webhook to `POST {tsm}/api/v1/audit/ingest` to federate its audit trail.
- [ ] Verify: TSM's `/suite/manifest` advertises `audit.ingest.v1`; the registry's "Consumed by" panel populates after a plan is ingested; a federated registry action appears in TSM's Audit logs marked `federated`.
