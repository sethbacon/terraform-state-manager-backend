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

Isolation is **not complete**, and this is the honest position rather than the
advertised one. Of the nine partition roots, two have scoped reads:

| Reads are organization-scoped | Reads still return every row |
| --- | --- |
| `state_sources`, `schedules` | `pipeline_connections`, `ci_sources`, `notification_channels`, `state_transfers`, `drift_runs`, `drift_records`, `health_runs` |

That is seven planes on which a caller still sees other organizations' rows.
The remaining flips are tracked by [#393][issue393]; do not read this page as
saying the application is isolated today.

One clarification the `schedules` flip earned. On that root the unscoped read
was not only a disclosure: `POST /schedules/{id}/run` loaded the schedule by id
and dispatched its target under the *schedule's* organization, so a caller in
another organization could execute it on that organization's pipeline
connection. Where a root's reads feed a dispatch, an unscoped read is an
execution boundary and not merely a visibility one.

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
   wanted, on one plane so far. Track [#393][issue393] for the other eight.
