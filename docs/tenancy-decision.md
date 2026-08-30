# What organizations mean in the state manager

**Decided: the state manager is organization-isolated.** A member of one
organization does not see another organization's data. There is no
configuration that turns this off, and there is not going to be one.

This page exists because that decision had been made in a commit and nowhere
else (#502). The read flip for state sources shipped in **v3.13.0**, and the
configuration that could have restored the previous shared behaviour was
removed in the same change — so the behaviour was live and the reasoning was
not written down anywhere an operator would look.

## What this means for a shared fleet

If several teams are modelled as separate organizations and all of them expect
to see every state source, **that deployment has already changed behaviour**.
Each team now sees only its own.

The supported way to run a shared fleet is to **model it as one organization**.
Teams are then divided by roles and scopes within that organization, which is
what organizations are for here: access division, not content partitioning.

The only other remediation available today is a `platform_admins` row, which
grants deployment-wide visibility. That is a real answer for an operator, not a
model for a team.

## Why it is this way

Per the [estate tenancy model][model], the **host** is the content tenant.
Modules, providers and binaries belong to a host; a second host is a second
deployment. The state manager is **single-host by design** — there is no host
column and there should not be one — and organizations divide access *within*
that single host.

So "organizations are isolated from each other" is not a tightening bolted onto
a shared design. It is what an organization already meant, finally enforced.

[model]: https://github.com/sethbacon/terraform-suite-identity/blob/main/docs/tenancy-model.md

## Where the migration actually stands

**All nine partition roots now read through an organization-scoped reader.**

| Reads are organization-scoped | Reads still return every row |
| --- | --- |
| `state_sources`, `schedules`, `pipeline_connections`, `ci_sources`, `drift_runs`, `drift_records`, `health_runs`, `notification_channels`, `state_transfers` |  |

The right-hand column is empty, and that is the finished state of the Phase 3
read flip tracked by [#393][issue393]. **It is not the same claim as "this
application is tenant-isolated"** — see [what is still
open](#what-a-closed-read-predicate-does-not-close) below, which is the part of
this page an operator should read before deciding anything.

The last two roots landed together, and neither was blocked on a dependency. An
earlier version of this page said `notification_channels` was held because the
shared notification library could not carry an organization; that was inherited
from a stale note and was false.

`notification_channels` had all **three** sides of the partition open at once.
Its **delivery** path was already scoped — the shared library exposes
`WithOrgScope` as a channel query option, `Notify` forwards those options to
`ListEnabledForEvent`, and this application passes `notify.ForOrganization` at
every `Notify` call site — so a notification for one tenant was never delivered
to another's channel. The **CRUD** surface was the gap: `ListChannels` showed an
operator every organization's channels, and the update, delete and test-send
found their row by id alone. That last one is not a listing problem. A channel's
`encrypted_target` is a capability-bearing secret (a Slack or webhook URL,
or an SMTP recipient list), the by-id read returns it, and the test-send
*decrypts it and POSTs to it* — so an unscoped test-send was one tenant making
this deployment deliver to another tenant's webhook, and an unscoped update was
one tenant redirecting another's alerts. All four now resolve a scope, and every
`/notifications/channels` route carries `TenantScope`.

`state_transfers` is the deliberate **two-organization** case 000033 calls a
supported capability, and the scoped read does **not** try to serve both ends of
a move. The row records the **acting** organization by design. The write path
already requires the caller to hold authority on both ends — it loads each end
through the scoped source reader, the target *before* its credentials are
decrypted — and the counterparty organization receives its own audit entry so a
transfer out of it is not invisible to it. Admitting the counterparty to the
scoped read instead would show one organization the other's source ids and state
keys, which is most of what a transfer record consists of; deriving the row's
organization from the source instead of the caller would hide a transfer from
the organization that performed it. The audit entry is the mechanism for "the
counterparty needs to know", and widening a tenant predicate is not.

## What a closed read predicate does not close

Read this section before telling anyone this deployment is isolated. Nine of
nine roots scoped means **no read path serves another organization's row of a
partitioned table to a caller who resolved a scope**. Four things sit outside
that sentence, and three of them are deliberate.

**Enumeration is still deployment-wide, on purpose.** `GetDue` and its siblings,
and the statesync reconcile loop, walk every organization's rows because finding
due work across the fleet is the system's job. What is scoped is what happens
*next*: every per-item load runs under an authority derived from the row that was
enumerated (`internal/tenancy.SystemActingIn`). The HTTP-triggered ones are
recorded per method in `internal/api/unscoped_twin_class_test.go`'s
`justifiedUnscoped`, where an exemption names one method and cannot silently
cover its neighbours. **The background ones are not recorded anywhere**: that
scan parses `internal/api` only, so `statesync`'s fleet-wide reconcile is
justified by the same reasoning and checked by nothing. That gap is stated in
the guard itself and is a known follow-up, not a claim that it does not exist.

**The machine-callback lookups precede their own authority.** The read that
identifies the run a callback token belongs to cannot run under a scope, because
that run is where the scope comes from. Also in `justifiedUnscoped`, also
per-method.

**An API key minted before the acting-organization fix is bound to the wrong
organization.** A key's request is scoped to the organization the key itself
carries, not to the union of its owner's memberships — which is the correct,
narrower answer. But keys minted before `mintKey` learned to stamp the *acting*
organization all carry the deployment's default one, and **there is no
backfill**: their organization is a fact about when they were created rather than
about where they are used. In a single-organization deployment that is right by
coincidence. In a multi-organization one, a legacy key belonging to someone who
works elsewhere binds to the default organization. **Rotating the key re-mints it
against the acting organization and fixes it**, and that is the only remedy —
nothing in this work changes it.

(An API key with *no owning user* is a different matter and is not a gap: such a
key is refused outright at authentication, along with one whose owner no longer
exists and one bound to no organization. That was closed separately.)

**The write side is scoped separately, and is not what this table describes.**
Mutating statements on the roots go through the `InScope` mutators and
`scopeWrite`; INSERTs are stamped with the acting organization. Those landed
across other increments, and this page's table is about reads.

### The machine callbacks are scoped too, and not by a middleware

`drift_runs`, `drift_records` and `health_runs` are read by two kinds of caller,
and only one of them is a person. A CI job posts its plan result to
`/api/v1/drift/runs/{id}/results` carrying a per-run bearer token and nothing
else — no session, no membership, no organization. There is no tenancy to
resolve for a request like that, so those two callback routes deliberately carry
no `TenantScope` middleware.

Their authority comes from the credential instead: the token authenticates one
run, the run carries its own `organization_id`, and that organization derives a
single-organization scope which every statement afterwards runs under — the same
scope type, and the same SQL predicates, the request path uses. A callback
authenticated for a run in one organization therefore cannot read or write a
drift record or health run in another, and a run that belongs to no organization
confers no authority at all rather than a deployment-wide one.

One clarification the `schedules` flip earned. On that root the unscoped read
was not only a disclosure: `POST /schedules/{id}/run` loaded the schedule by id
and dispatched its target under the *schedule's* organization, so a caller in
another organization could execute it on that organization's pipeline
connection. Where a root's reads feed a dispatch, an unscoped read is an
execution boundary and not merely a visibility one.

The `pipeline_connections` and `ci_sources` flips carried the decision that
question forced ([#393][issue393], option B): **background work acts under a
derived tenant scope**. The scheduler has no request and no principal, so for
each schedule it fires it derives "system, acting in organization X" from the
schedule row itself (`internal/tenancy.SystemActingIn`, provenance included)
and every by-id load on the dispatch chain — the pipeline connection, the
target state source, and the CI source whose *shared credential*
`resolvePipelineToken` decrypts — is an `InScope` read under that one-organization
scope. A chain that crosses organizations fails closed and logs the row that
led there. Enumeration (`GetDue` and its siblings) stays deliberately unscoped:
finding due work across organizations is the system's job; acting on an item is
scoped to that item's owner. Write-side, a schedule or connection that
references a row in another organization is refused at write time.

That table is **checked against the code, not maintained by hand**.
`internal/tenancy/scoping_status_test.go` declares the status of every root,
checked against `PartitionedTables` in both directions, and prints the current
split in its test output. Adding a partition root without deciding its scoping
status fails the build.

The table on this page is parsed by that same test and compared to the
declaration, in both directions, so the two cannot disagree: a flip that updates
the code and forgets this page fails the build, and so does the reverse. That
guard was added when the `schedules` flip found this page still claiming one
scoped root — a second hand-copy of an inventory is the same hazard as the first
one, and this page previously asserted it was not hand-maintained while being
exactly that.

[issue393]: https://github.com/sethbacon/terraform-state-manager-backend/issues/393

## If you are upgrading past v3.13.0

1. Run the census in [organization-reown.md](organization-reown.md) — it tells
   you how many organizations you actually have.
2. **One organization:** nothing to do. Isolation between one organization and
   itself is not observable.
3. **More than one, and they expect shared visibility:** consolidate onto one
   organization, or accept per-organization visibility and re-own the rows to
   the organizations that should hold them.
4. **More than one, already expecting isolation:** this is the behaviour you
   wanted, and the read predicate is now closed on all nine partition roots.
   Read [what a closed read predicate does not
   close](#what-a-closed-read-predicate-does-not-close) before treating the
   deployment as isolated.
