<!-- markdownlint-disable MD013 -->
# 5. Per-App Authorization Tables

**Status**: Accepted

## Context

[ADR 004](004-role-seed-ownership.md) elected a single role-seed owner because `identity.role_templates.name` is globally `UNIQUE` and both suite apps define roles by the same names with different scopes, so each restart overwrote the other's. That ADR fixed the symptom and named the cause in its own Consequences: under a shared store the non-owner's role definitions become *advisory*, effective scopes depend on which app owns the seed, and role changes shipped by the non-owner do not take effect until the owner ships them too.

The cause is that authorization is stored where identity is stored. The State Manager owns **no** identity tables — it constructs the shared library's repositories against the identity connection and uses them directly — so "the editor role" and "alice is an editor of acme" live in `identity.role_templates` and `identity.organization_members`, which the sibling registry reads and writes as well. `TSM_SUITE_ROLE_SEED_OWNER` exists solely to arbitrate a collision that only exists because two applications share one authorization table.

The agreed suite-wide model is [`sethbacon/terraform-suite-identity#206`](https://github.com/sethbacon/terraform-suite-identity/issues/206): **identity is shared, authorization is per-app.** Membership stays a fact in `identity.organization_members`; which role that member holds *in this application* moves into this application's own schema. The platform-admin carrier ([migration `000030`](../../backend/internal/db/migrations/000030_platform_admins.up.sql)) was the first piece of that model to land here.

## Decision

The State Manager gets its own `role_templates` and `organization_member_roles` on the **application** connection (migration `000032`), written in lockstep with the identity tables and reconciled from them at every startup.

**No foreign keys to identity.** `organization_member_roles.(organization_id, user_id)` name identity rows and carry no constraint, because identity may be another schema *or another database* (`TSM_IDENTITY_DATABASE_*`) where a foreign key is not expressible. The foreign key to this app's own `role_templates` **is** expressible and is taken, with `ON DELETE SET NULL` mirroring identity's own column exactly.

**The mirror is structural, not conventional.** `approles.Members` embeds the shared organization repository under an *unexported type alias*, so promoted reads keep working unchanged while the unwrapped repository is unreachable from outside the package. Every method that can set, change or remove a role is overridden to write both places. A new assignment path cannot forget to mirror; it has nothing to call that does not. `approles/dual_write_class_test.go` refuses to certify a tree where the shared repository is constructed anywhere else, where a wrapper method writes one side and not the other, or where a user hard-delete is not paired with a mirror purge.

**Ordering rule**: the two legs are on connections that cannot share a transaction, so the writes are ordered such that a crash between them leaves the *less privileged* state — grants write identity first, revocations write the mirror first.

**Tenancy lives in the SQL.** Every statement over `organization_member_roles` binds the caller's `OrgScope` through `OrgScope.SQL`, which never returns an empty clause: the platform-wide scope is a literal `TRUE`, an undecided caller's zero value is a literal `FALSE`. A first attempt checked the scope in the layer above and left the statements unqualified; that closes the paths which remember and leaves the data layer unable to refuse the ones that do not, which is the shape the library's own #138/#162 rejected. The suite's tenant-scope signature found it on the branch's first CI run.

**The credential sweep is a mandatory argument to the mutation**, not a statement the caller runs afterwards — `AuthorityReducer`, the same inversion `identity/platformadmin.AuditIntentWriter` uses. TSM's two credential families freeze a principal's authority at issue time, so a role write that does not sweep takes nothing away (#330); making the sweep a parameter means the reduction cannot be spelled without it, while the *flavour* stays the caller's, which is what preserves the deliberate keys-only asymmetry on the IdP login path.

**The backfill copies what the app resolves today, not what it defines.** `approles.Reconcile` copies `identity.role_templates` verbatim, ids included, and restates every membership. In a standalone deployment that is this build's own seed, written moments earlier. In a coupled one it is the sibling's definition — which is what this deployment authorizes against right now, and Phase 3a must not change authorization. The divergence is reported as a startup warning rather than corrected.

**Reads do not move.** Every authorization decision still comes from `identity.organization_members` joined to `identity.role_templates`. Nothing observable changes.

## Consequences

**Easier**:

- The collision ADR 004 manages stops existing once reads move: two apps can each have their own `admin` with their own scopes, because `name` is now unique *per app*.
- Role definitions shipped by this app take effect in this app, decoupling the two apps' release cadence for roles.
- Startup reconciliation makes the mirror self-healing: a failed mirror write, a `CASCADE`, or a row written by the sibling all converge rather than accumulating.
- The routing guard (migration pre/post-checks plus `Store.Verify`) turns "the app connection is pointed into the identity schema" from an invisible re-collision into a startup failure that names what it resolved.

**Harder**:

- Two writes on two connections, with a window between them. It is unobservable while reads stay on identity, and bounded by the reconcile, but it is real and the ordering rule is the whole of its mitigation.
- A mirror failure cannot fail the request (the identity leg has already committed), so divergence is reported through logs and `approles.DriftQuery` rather than surfaced to the caller.
- Handler constructors now take the application connection as a required parameter. That is deliberate — an option can be omitted and an omitted mirror is silent — but it touches every construction site.

**Unchanged, for now**: `TSM_SUITE_ROLE_SEED_OWNER` still governs the shared seed and is still load-bearing, because reads still come from the table it arbitrates. Reads move in [ADR 006](006-per-app-authorization-reads.md), which found this paragraph's prediction that the flag "becomes inert" to be **wrong**: it also gates the setup wizard's identity-ownership decisions, and the shared seed it arbitrates is still read by the sibling registry and by ADR 006's rollback path. It is removed with the shared authorization surface in the final phase of #206. ADR 004 stays **Accepted** until then.
