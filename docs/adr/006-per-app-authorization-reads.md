<!-- markdownlint-disable MD013 -->
# 6. Per-App Authorization Reads

**Status**: Accepted

## Context

[ADR 005](005-per-app-authorization-tables.md) gave the State Manager its own `role_templates` and `organization_member_roles` on the application connection, dual-written with the identity tables and reconciled from them at every startup. It deliberately stopped short of the thing that matters: **reads did not move.** Every authorization decision still came from `identity.organization_members` joined to `identity.role_templates`, so the new tables were a mirror nobody looked at.

That is Phase 3a of [`sethbacon/terraform-suite-identity#206`](https://github.com/sethbacon/terraform-suite-identity/issues/206). This ADR is Phase 3b, and it is where the risk is.

**A gap in the dual write does not surface as an error.** It surfaces as a user silently holding the wrong role — losing access they should have, or keeping access they should not. The request succeeds, the response looks right, the audit entry is written. Nothing in the request path says otherwise. Phase 3a's own report called drift low-stakes *because nothing read the mirror*; that sentence stops being true here, and re-reading it is what produced most of what follows.

There is a second trap, specific to the mechanism ADR 005 chose. `approles.Members` embeds the shared repository, and **Go has no virtual dispatch**. The library implements `GetUserCombinedScopes` as a call to *its own* `GetUserMemberships`, `GetUserScopesForOrg` as a call to *its own* `GetMemberWithRole`, and `OrgScopeForUser` and `CheckMembership` likewise. Overriding the base reads and leaving the derived ones promoted produces a repository whose membership list comes from this application's tables and whose **session scopes** come from identity's — a principal whose `/auth/me` shows one role and whose token grants another. It compiles, and it passes every test of either method taken alone.

## Decision

**Role reads answer from this application's tables.** `approles/reads.go` overrides every role-carrying accessor. Membership stays identity's fact: each override calls the shared method it replaces, unchanged and with the caller's `OrgScope`, and then **overlays** the role columns from `organization_member_roles` joined to this application's `role_templates`. Rewriting the queries was the alternative and is rejected — the library's scope handling is where #138/#161/#162 were found and fixed, and a second hand-rolled copy here is a second place for them to come back. An overlay cannot widen a read; it only decorates rows the scoped identity read already returned.

**A gap fails closed.** A membership identity has with no row in `organization_member_roles` resolves to no role and no scopes, with *every* role field cleared. That is the safe direction: the principal loses access they should have, which is loud and self-reporting, rather than keeping access they should not, which is silent.

**The flip is gated on provable equivalence, not on a release note.** `approles.CheckDrift` compares the two sources and `tsm-server authz-drift` exits non-zero while anything is unreconciled — the shape `bind-secrets verify` and `rekey-targets verify` already established here. It replaces Phase 3a's `DriftQuery` for programmatic use because that statement cannot see two cases that matter now: it does not run at all when identity is a separate database (`TSM_IDENTITY_DATABASE_*`), and it compares `role_template_id` and nothing else — so two rows agreeing on the id while the two schemas define that id with **different scopes** read as clean, which after the flip is exactly a principal holding the wrong permissions under the right role name.

**Divergence after the flip is detected by a periodic comparison, not by a shadow read.** The same `CheckDrift` runs on an interval (`TSM_AUTHZ_DRIFT_INTERVAL`, default 15m), exports `tsm_authz_role_drift{kind=...}` and logs. A shadow comparison in the read path was the other candidate and is worse here: it runs on the API-key hot path, which re-derives a key owner's live scopes on *every* request, and it only ever sees principals who authenticate — a service account silently holding an administrator role it should have lost stays invisible until somebody uses the credential. It **reports and never corrects**; correcting is the reconcile's job, at boot, where an operator can put a boundary around it.

**The rollback is a restart.** `TSM_AUTHZ_ROLE_SOURCE=identity` puts every role read back on the shared schema. It works because the dual write is *not* conditional on the setting — both places are still written under either value, so the source that is not being read stays current rather than going stale — and because this phase drops nothing.

**This application defines its own roles.** `bootstrap.seedRoleTemplates` now writes `auth.AppRoleTemplates()` into this application's `role_templates`, unconditionally: `name` is unique *per application* here, so there is no sibling to collide with and no configuration that should be able to leave these roles undefined. The identity-side seed stays, still gated by `suite.role_seed_owner`, for two reasons that both expire in Phase 4 — the rollback path reads `identity.role_templates` (a build that stopped seeding it would leave a *fresh* standalone deployment unable to roll back at all), and the sibling registry still authorizes from it.

**The reconcile's template pass becomes adopt-if-absent.** In Phase 3a it upserted identity's definitions over this table every boot, which was correct while identity's definitions *were* the effective ones. Now they are not, and an upsert would let the shared schema — in a coupled deployment, the sibling registry — silently redefine what a State Manager role grants, once per restart, on the table that decides authorization. Identity may still **supply** a definition this deployment has never seen (without one, an assignment referencing it would violate the foreign key and the principal would lose their role); it may no longer **redefine** one. Adopt runs *before* the seed, so identity's uuid comes across and the seed then replaces that row's scopes by name while leaving its id alone — reversed, a fresh install mints its own uuid, adoption finds identity's under a different one, the name is released, and this build's scopes are silently replaced by identity's.

**The reconcile's repairs became observable.** It still repairs, and turning it into a reporter would be wrong: `identity.organization_members` rows disappear by `ON DELETE CASCADE` with no statement this application ever sees, and the sweep is the only thing that withdraws the matching authority here. But every repair is now an authorization change made by a batch job at boot — a `missing` row it fills in **grants** access, a `stale` row it sweeps **revokes** it — so the comparison runs first and its result is logged. The reconcile says what it is about to change, before it changes it.

**Two class guards keep the read set complete**, and their universe is *derived from the library's own source* rather than hand-listed: a list is correct the day it is written and silently wrong the day `terraform-suite-identity` adds an accessor, which is the upgrade during which nobody re-reads the guard. `TestEveryRoleCarryingReadIsOverridden` parses the module in the build's module cache, resolves the shared query constants (since `membership.go` the role JOIN lives in a constant, not in any method body), closes the role-carrying set over the methods that call one another, and requires `Members` to declare each. `TestNoOverriddenReadIsAPureForwarder` then requires each override to actually consult this application's tables.

## Consequences

**Easier**:

- This application's roles mean what this build says they mean. A role change shipped here takes effect here, on any topology.
- The `TSM_SUITE_ROLE_SEED_OWNER` collision is gone *for this application's own reads*: two apps can each have their own `admin` with their own scopes.
- One comparison serves three jobs — the pre-flip gate, the boot-time report of what the reconcile is about to change, and the standing detector — so they cannot disagree with one another.

**Harder**:

- **A coupled deployment's authorization changes.** Where `suite.role_seed_owner` is not `self` or `tsm`, this deployment has been authorizing against the sibling's definition of every role name; from the first boot on this build it authorizes against its own. That is the correction #206 exists to make and the one deliberate behaviour change in this phase, but it is a behaviour change, and it is the reason the phase carries a breaking-change footer. A standalone deployment (the default) sees no change at all, and the equivalence proof is what establishes that.
- Role reads now issue a second query against the application connection. It is one bulk query per call, not per row, and is bounded by the same page the identity leg returns.
- The role source is a required constructor parameter (`approles.NewMembers`), so every construction site had to state its answer. That is the point — an optional guard is how a guard goes silently absent — but it touches every construction site, as ADR 005's `appDB` parameter did.

**Unchanged, for now**: nothing is dropped. `identity.organization_members.role_template_id` and `identity.role_templates` stay, still dual-written, because the rollback path and the sibling registry both read them. `TSM_SUITE_ROLE_SEED_OWNER` **cannot** be retired here: besides the shared seed it also gates the setup wizard's identity-ownership decisions (`ConfigureAdmin`, `SaveOIDCConfig`, `CompleteSetup`), which are about who owns identity rather than about the role-template collision. ADR 004 stays **Accepted**; both it and this ADR's residue are settled in the final phase of #206.
