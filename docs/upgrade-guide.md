# Upgrade guide

## How upgrades work

The backend embeds its migrations and applies them on boot (app schema, then
the shared identity schema) under PostgreSQL advisory locks — concurrent
replicas can't race. Old replicas keep serving during a rolling upgrade;
migrations to date are additive.

Imperative control when you want it:

```bash
terraform-state-manager migrate up      # apply before rolling images
terraform-state-manager migrate down    # one step back (verify the matching .down.sql first)
```

## Procedure (any platform)

1. Take/verify a DB backup ([disaster-recovery.md](disaster-recovery.md)).
2. Read release notes for env-var or values changes.
3. Bump image tags (helm `--set backend.image.tag=… frontend.image.tag=…`,
   kustomize `images:`, compose `.env.production`).
4. Roll backend first, frontend second (the SPA is tolerant of a newer API;
   the reverse direction may reference endpoints an old backend lacks).
5. Verify `/health`, `/ready`, `/api/v1/version`, dashboard data, and that
   the worker logs a sync cycle.

Rollback = redeploy the previous tag. Schema rollbacks are rarely needed
(additive migrations); if a release notes otherwise, it ships explicit steps.

## One-time notice: state sources are confined to configured roots

The release that introduces `statesource.local_roots` confines the two connector
types that name a path on the **server's own filesystem** — `local` (`base_path`)
and `kubernetes` when it is given a `kubeconfig` file — to directories the
operator lists. The lists **fail closed**: empty (the default) permits nothing,
so an existing `local` source stops resolving the moment the new binary boots
unless its directory is listed.

Before rolling this release, if you use `local` sources (or a kubeconfig-based
`kubernetes` source):

- Note each source's `base_path` (`GET /api/v1/sources`).
- Set `TSM_STATESOURCE_LOCAL_ROOTS` to the directory (or directories) that
  contain them — the mount point is usually the right entry, e.g.
  `/data/states`, not each individual sub-path. Helm sets this automatically
  from `localStates.mountPath` when `localStates.enabled=true`.
- Set `TSM_STATESOURCE_KUBECONFIG_ROOTS` likewise for a mounted kubeconfig, or
  switch that source to `config.server` + `credentials.token`.
- Roll, then confirm the source lists states (`POST /api/v1/sources/{id}/test`).

Nothing is deleted and no source record changes — a source whose root is missing
simply reports that its `base_path` is outside the permitted roots until you add
it. Deployments that read state only from remote backends (HCP, S3, Azure Blob,
GCS, Consul, PG, HTTP, Git) need no action. See
[configuration.md](configuration.md#state-sources).

## One-time notice: automatic state-backup retention

Installs upgrading **to the release that introduced `backup_retention`** get the
sweep **enabled by default**, and its first worker cycle after the upgrade will
delete existing backups that fall outside the policy — older than 90 days *and*
not among the newest 20 for their state. On an install that has been accumulating
backups since day one, that first sweep can remove a large number of rows.

Deletion is permanent; the sweep has no undo. Before rolling this release:

- Confirm your DB backup (step 1 of the procedure above) is current — that is the
  only recovery path for pruned rows.
- If you have retention obligations that forbid automatic deletion, set
  `TSM_BACKUP_RETENTION_ENABLED=false` **before** rolling the worker replica.
- To keep the sweep but start gentler, raise `TSM_BACKUP_RETENTION_KEEP` or
  `TSM_BACKUP_RETENTION_MAX_AGE` for the first cycle, then tighten.

The sweep runs only on the worker leader (`workers.enabled=true`), so an
API-only replica rolling first changes nothing. See
[configuration.md](configuration.md#workers) and
[capacity-planning.md](capacity-planning.md).

## One-time notice: authority changes now end sessions

Starting with the release that adds the `user_token_revocations` table, an event
that **reduces** a principal's authority invalidates the credentials that froze
the old authority, instead of leaving them working:

- **Sessions.** Removing a member from an organization, changing a member's role
  template, deleting an organization, deleting or erasing a user, and every SCIM
  deactivation now retire that user's existing JWT sessions immediately. The
  affected user sees a `401` and must log in again — including after a *promotion*,
  because the session is retired whichever way the role moved. Nobody else is
  affected; the watermark is per user.
- **API keys.** The same events revoke API keys whose stored scopes the user's
  remaining authority no longer grants (offboarding revokes all of them). This is
  **permanent** — a key's secret is displayed once at creation — so a key revoked
  by a role change must be re-created, not recovered.

As of 3.1.0 the key sweep covers **every** key the principal holds, not only the
keys stamped with the organization whose membership changed. Every API key in
this application is stamped with the default organization regardless of who owns
it, so an organization-filtered sweep matched almost nothing and left the very
credentials it was meant to retire working. Expect a role change to retire more
keys than the previous release did.

No configuration is involved and nothing needs to be set before rolling: the
migration is additive and the behaviour is on as soon as the new backend serves.
Expect a burst of re-logins if you roll during a bulk membership or role change.

## One-time notice: identity traffic is confined to an egress allow-list

Starting with 3.1.0 the requests the shared identity module makes on this app's
behalf — OIDC discovery, the JWKS fetches that decide which ID tokens are valid,
the authorization-code token exchange, and the sibling-app manifest poll — go
through the same egress guard the state-source connectors use. Their default is
**strict deny**: loopback, RFC1918, link-local, CGNAT and IPv6 ULA are all
refused, because an IdP is pinned by URL and a deployment whose one is internal
can say so.

If your IdP or sibling app lives on an internal address (every self-hosted
Keycloak/ADFS, and every compose stack), set `TSM_SECURITY_EGRESS_ALLOWLIST`
**before** rolling — otherwise the server fails at startup naming the denied
endpoint (`egress to "<host>" blocked`). `DEV_MODE` does not cover this: the
scheme rule and the destination rule are separate controls.

The trap is that this one setting feeds **both** consumers, and setting it
**replaces** the connectors' built-in private-range default rather than widening
it. A value added only to admit an internal IdP silently withdraws RFC1918 from
the state-source connectors, and every internal state backend stops resolving. A
deployment that needs both must re-state the private ranges:

```
TSM_SECURITY_EGRESS_ALLOWLIST=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7,<idp-host>
```

Prefer the IdP's **hostname** over its CIDR — narrower, and it survives the host
getting a different address. Deployments whose IdP is public and whose state
backends are all remote (HCP, S3, Azure Blob, GCS) need no action. See
[configuration.md](configuration.md).

## One-time notice: read-then-mutate races now answer 404

Requests that read a record and then write it — on users, organizations,
memberships, API keys and OIDC configs — used to answer with a success status
when the record vanished between the two steps, reporting a write that never
happened. From 3.1.0 they answer `404`.

Nothing needs configuring, but a client that treated `2xx` as "the change is
applied" will now see failures where it previously saw silent no-ops. Repeat
`DELETE`s are unaffected and keep their existing success codes.

## One-time notice: SMTP carries an explicit TLS mode

SMTP configuration now holds an explicit TLS mode whose **zero value requires
TLS**, and the mailer refuses to send credentials in the clear to a non-local
relay. Two consequences:

- A `PUT /api/v1/notifications/smtp-config` that **omits** `use_tls` no longer
  disables TLS. An omitted field leaves the current setting alone; send
  `"use_tls": false` to turn it off deliberately. Any automation that PUTs a
  partial config was previously downgrading the relay on every call.
- A relay configured with a username/password over plaintext to a non-local host
  is refused rather than attempted. If you rely on that, either enable TLS on the
  relay or drop the credentials.

## One-time notice: authority is bound to the presenting credential

**Breaking.** Three endpoints now derive what a caller may do from the
**credential presenting the request** rather than from the **owning user's role
rows**. Before this release an API key could reach authority its owner held but
the key itself never did, defeating the two narrowings the API-key path applies
deliberately (a key's stored scopes are capped by its owner's live set, and
`admin` is stripped from every API-key request unconditionally so an unattended
CI credential cannot inherit its owner's platform-admin).

| Endpoint | Now refused | Do this instead |
| --- | --- | --- |
| `POST /api/v1/auth/refresh` | API-key and mTLS callers get **403** | Nothing to change for browsers. A key has no session to refresh — rotate it at `POST /api/v1/apikeys/{id}/rotate`. |
| `PUT`/`DELETE /api/v1/admin/organizations/{id}` and its `/members` routes | Callers whose credential does not itself carry `organizations:write` or `admin` get **403**; **no API key can**, as neither scope is key-assignable | Manage organizations from an interactive session. |
| `POST /api/v1/apikeys/{id}/rotate` | Rotating a key whose scopes the caller does not hold gets **403** | Rotate from a session, or from the key itself. |

Who is affected, in practice:

- **Browser/SPA users: nobody.** The frontend never calls `/auth/refresh`, and
  organization management already runs from a session.
- **Automation holding a `tsm_` API key** that called `/auth/refresh` to obtain a
  session cookie. This was the escalation: the resulting cookie carried the
  owner's whole cross-organization scope union, `admin` included. Point that
  automation at the API directly with its key — the key already carries the
  scopes it was granted — or give it a key with the scopes it actually needs.
- **Automation that rotated a *different* key than the one it authenticates
  with.** A key may still rotate itself, and an `admin` session may still rotate
  anyone's key; what is refused is a narrow key rotating a broader sibling of the
  same owner and receiving its secret.
- **An owner whose own authority was reduced** since a key was minted can no
  longer re-mint that key at its original breadth. The key was already capped at
  the reduced set on every request, so this only stops the stale breadth being
  written back into a fresh row. Recreate the key with the scopes you now hold.

Check before rolling: `GET /api/v1/apikeys` lists every key with its scopes and
`last_used_at`. A key whose scopes exceed its owner's current authority, or whose
automation refreshes a session, is one to re-issue first. Nothing is revoked and
no schema changes.

## One-time notice: an mTLS mapping granting `admin` must name a user

**Breaking, and it fails at startup rather than at request time.** It affects you
only if `auth.mtls.enabled` is true AND one of your `auth.mtls.mappings` lists
`admin` among its scopes.

An mTLS subject mapping used to publish its configured scopes verbatim, so
`scopes: ["admin"]` made the certificate holder a platform administrator directly
from the config file — with no `platform_admins` row, no record of who granted
it, no audit entry, and no revocation short of editing configuration and
restarting. Sessions and API keys had already been brought under the carrier;
this was the last path answering "who is a platform administrator?" from
somewhere else.

A mapping carrying `admin` now has to name the user the certificate acts as:

```yaml
auth:
  mtls:
    mappings:
      - subject: "CN=break-glass"
        scopes: ["admin"]
        user_id: "3f1c9a02-6d4e-4a1b-9f77-2b8e5c0d1a44"   # NEW, and required for `admin`
```

`user_id` is optional for every other mapping — an ordinary machine credential
needs no user behind it — and nothing changes for those.

Naming a user does not by itself grant anything. On every request the carrier is
consulted for that user, exactly as it is for a browser session, so `admin` holds
only while they hold a carrier row and revoking it disarms the certificate on the
next request rather than at the next restart.

A repeated `subject` is now refused too. It previously took the last mapping in
the file silently, which would now bind a certificate to a different principal
than the line you are reading.

**What you will see if you are affected.** The server refuses to start, naming
the subject:

```
failed to initialise mTLS provider: auth.mtls.mappings: subject "CN=ci" carries
the `admin` scope with no user_id. Platform administration is held in the
platform_admins carrier, which is keyed on a user; set user_id to the UUID of the
user this certificate acts as (and grant them platform administration), or remove
`admin` from the mapping
```

**Before you deploy.** Search your config for `mtls`. If `enabled` is false or
absent, nothing here applies. If any mapping lists `admin`, decide who that
certificate acts as, confirm they hold a carrier row (`GET
/api/v1/admin/platform-admins`), grant it if not, then add `user_id` to the
mapping. Removing `admin` from the mapping is also a complete answer if the
certificate did not need platform-wide reach.

## One-time notice: `drift_runs` gains two indexes on upgrade

Migration `000037_drift_runs_batch` adds `batch_id`/`ci_run_id`/`ci_run_url`
(nullable, no default — catalog-only, no table rewrite) plus two plain
`CREATE INDEX` statements. golang-migrate runs each migration file as one exec,
and PostgreSQL runs a multi-statement exec inside an implicit transaction, so
`CREATE INDEX CONCURRENTLY` — which cannot run inside a transaction — is not an
option unless it is the only statement in its own file.

For most installs this is a non-event: `drift_runs` is small and a plain
`CREATE INDEX` takes a brief `SHARE` lock that blocks writes for the build's
duration only. An operator with a **very large `drift_runs` (over roughly one
million rows)** should pre-create both indexes `CONCURRENTLY` by hand before
rolling this release, so the migration's own `CREATE INDEX IF NOT EXISTS` finds
them already built and does nothing:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_drift_runs_batch
    ON drift_runs (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_drift_runs_state_created
    ON drift_runs (source_id, state_key, created_at DESC);
```

## Version pinning

Always pin image tags in production (`v1.0.0`, never `latest`); the chart's
`appVersion` is only the default tag.
