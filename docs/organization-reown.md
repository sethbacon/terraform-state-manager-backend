# Re-owning the organization partition

> **Read [tenancy-decision.md](tenancy-decision.md) first** if you are upgrading
> past v3.13.0. It states what organizations mean here — isolation, not a shared
> fleet — and how far that isolation actually reaches today. This page is the
> mechanics; that page is whether you need them.

Rows written before the acting-organization stamp shipped all sit at the
deployment's **default organization**. This is how you move them to the
organization that actually owns them.

If your deployment has only ever had one organization, you do not need this.
Run the census, confirm that, and stop.

---

## Why this is a command and not a migration

`tenancy.Backfill` runs at every boot and sweeps `WHERE organization_id IS NULL`.
After the first boot there are no NULLs, so it never repairs anything again. The
rows are non-NULL and wrong, which is precisely the state it cannot see.

**Which rows belong to which organization is not derivable.** The five
configuration roots — `state_sources`, `pipeline_connections`, `ci_sources`,
`schedules`, `notification_channels` — carry `created_at` and nothing else. No
actor, no `created_by`, no owner. There is no provenance in the data from which
to compute an answer.

That is why this is not a migration. A migration runs itself, on every
deployment, with whatever mapping was compiled into it — so a mapping that is
right for one estate would be applied unasked to every other one. **The mapping
is an input**, supplied per deployment, by someone who knows the answer for that
database.

---

## Preconditions

Check these before running anything. Two of them have caused real incidents.

| Precondition | How to check | Why |
|---|---|---|
| **The deployed build has the acting-organization stamp** | `curl -s $TSM_URL/api/v1/version`, and confirm the tag contains the partition-root work | Re-owning rows on a build that still writes new rows at the default organization means the estate drifts back the moment traffic resumes. Reasoning from `main` about a live system has produced a wrong recommendation here twice. |
| **You have a database backup you have restored from at least once** | your own runbook | `Move` runs in one transaction and is idempotent, but a *wrong mapping* is not an error the database can catch. It will faithfully move rows to the organization you named. |
| **You know the organization UUIDs** | `SELECT id, name, slug FROM identity.organizations ORDER BY name;` | Both are required arguments. There is no default and no "everything except X". |

---

## Step 1 — census (read-only)

```bash
tsm reown-roots verify
```

Reports, for all nine partition roots, how many rows each organization owns.
It writes nothing. Run it on production freely.

Read the output before deciding anything:

- **One organization owning everything, and it is the right one** — you are done.
- **One organization owning everything, and it is the wrong one** — Step 2.
- **Rows already split across organizations** — Step 2 moves *one* source
  organization at a time. If the split does not follow organization boundaries,
  see [Splits this command cannot express](#splits-this-command-cannot-express).
- **`NULL` owners** — unexpected. `tenancy.Backfill` should have swept these at
  boot. Investigate before moving anything; a NULL owner is invisible to every
  tenant, because `NULL = ANY(...)` is NULL and never true.

## Step 2 — move

```bash
tsm reown-roots move <from-org-uuid> <to-org-uuid>
```

Both arguments are required. The command refuses to run if either is missing, if
they are the same, or if the destination does not name a real organization.

What it does, in this order and in **one transaction**:

1. **The five configuration roots** — every row owned by `<from>` is re-owned to
   `<to>`. This is your mapping being applied.
2. **The four dependent roots** — `state_transfers`, `drift_records`,
   `drift_runs`, `health_runs` — take their owner **from their parent**, which
   has just moved.

The order is not cosmetic. Deriving before the parents move computes every
child's owner from a parent that is still wrong, and the result is
indistinguishable from a correct one, with no NULL left to mark it.

### If the move empties the default organization

`move` reports this, and **does not fix it**:

```
WARNING: the rows moved OUT of this deployment's default organization
(<uuid>), and that setting is unchanged.
```

The default organization is where things land when nothing else decides:

- a **first login** not covered by an OIDC or SAML group mapping places the user
  there (`assignRole`, and the `default_role` fallback in group reconciliation);
- every partition root's column `DEFAULT` is `tsm_default_organization_id()`, so
  anything still relying on it writes there.

So after moving the estate to another organization, new users arrive somewhere
that owns nothing. Repoint the setting if the destination is now the organization
this deployment is really for:

```sql
UPDATE system_settings SET default_organization_id = '<destination-uuid>'::uuid,
       updated_at = now()
WHERE id = 1;
```

**The command deliberately does not do this for you.** Which organization a
deployment considers its default has effects beyond these nine tables, and
repointing it while re-owning rows would be two decisions taken under one
command. It is reported so you can make the second one deliberately.

## Step 3 — census again

```bash
tsm reown-roots verify
```

Confirm the distribution is what you intended.

---

## What it deliberately does not do

### Rows whose parent was deleted

`drift_records`, `drift_runs` and `health_runs` reference their parent
`ON DELETE SET NULL`. A row whose parent is gone has nothing to derive an owner
from, so it is **counted and left alone** rather than swept along — moving it
would be a guess wearing the same clothes as a computed answer. The move output
reports these as `left: parent deleted, owner not recoverable`.

`state_transfers` is not affected: its `source_id` is `NOT NULL ON DELETE
CASCADE`, so every surviving row has a living parent.

### Splits this command cannot express

It moves rows from one organization to another, wholesale. It cannot say "these
sources to A and those to B". For a row-level split, use the census to see what
is there and write the `UPDATE` by hand — then run
`tsm reown-roots move` for whatever remains uniform, or skip it entirely.

A predicate language here would be a guess at a requirement nobody has stated,
on a command that rewrites ownership of credentials.

### Anything outside the nine roots

The seven **inherited** tables — `state_backups`, `state_edits`, `state_locks`,
`state_analyses`, `source_sync_status`, `state_analysis_history`,
`state_module_refs` — have no `organization_id` at all. They reach their owner
through a `NOT NULL ON DELETE CASCADE` foreign key to `state_sources`, so moving
the source moves them by construction. Migration 000033 explains why duplicating
the column onto them would be a worse design rather than a safer one.

### API keys

`identity.api_keys` is not a partition root and this command does not touch it.
A key is tied to its owner, and its owner's membership determines where it may
act — enforced at authentication rather than by ownership of the row.

---

## Safety properties

- **One transaction.** A partial move is worse than no move: the configuration
  roots would have changed hands while their dependent records still pointed at
  the old owner, and the two are only consistent together.
- **Idempotent.** A second run is a no-op. An operator who is unsure whether the
  first run completed can simply run it again.
- **Contained.** Rows outside the named source organization are not touched.
- **The destination must exist**, checked against the identity schema on its own
  connection — organizations live there and the partition deliberately carries no
  foreign key into it, because identity may be a different database. Stamping
  rows into an organization that names nothing would produce well-formed rows
  invisible to every tenant and visible only to a platform admin: the exact
  failure the partition exists to close, reached through the tool meant to repair
  it.

Each of these is covered by an integration test against a real PostgreSQL, and
each was verified by breaking it and watching the test fail. A mock cannot see
any of them: it cannot tell an `UPDATE` that moved the right rows from one that
moved all of them, cannot evaluate the `IS DISTINCT FROM` that makes the derive
step idempotent, cannot enforce the foreign keys that make an orphan an orphan,
and cannot roll a transaction back.

---

## After the move

Re-owning the rows is a prerequisite, not the whole of tenant isolation. Reads
are still unscoped; scoping them is a separate phase, and it has a second half
that is easy to miss — five routes read the inherited analysis tables fleet-wide
without touching a partition root at all.

See [`docs/adr`](adr/) and migration `000033_organization_partition.up.sql` for
the full sequence.
