<!-- markdownlint-disable MD013 -->
# 7. Retire the Identity Role Source

**Status**: Accepted

## Context

[ADR 006](006-per-app-authorization-reads.md) moved every role read onto this application's own tables and shipped a rollback lever beside it: `TSM_AUTHZ_ROLE_SOURCE=identity` put every role read back on the shared identity schema. It could do that because the dual write from [ADR 005](005-per-app-authorization-tables.md) is not conditional on the setting, so the shared schema stayed current while nothing here read it.

The lever was correct on the day it shipped and it did its job: the flip was gated on `authz-drift` reporting zero, and an operator who found a role wrong had a restart, not a restore, between them and the previous behaviour. Nobody pulled it.

It became a problem for a different reason, in a different repository. `sethbacon/terraform-suite-identity#206` Phase 4 removes the shared authorization surface, and an early step is the registry ceasing to seed the shared `role_templates`. The registry cannot do that while another application's rollback depends on the table being current. This application did not read that table on any live path any more — its authorization comes from its own `role_templates`, and migration `000032` refuses to run at all if the app connection can reach identity's tables unqualified — **except through this lever**. While it existed, retiring the registry's seed would not have broken the lever visibly; it would have turned it from a working rollback into a silent downgrade to whatever the table last held, which is worse than no lever.

Two things made this hard to see, and are recorded so the next reader does not re-derive them. Each repository's code named the *other* as its reason to keep the shared surface. The registry's seed said "the state manager still reads it"; this repository's ADR 006 said "the sibling registry still authorizes from it". The first was true when written and false once this application's Phase 3 landed. The second was imprecise rather than false: the registry authorizes from its own table, but its startup reconcile still *derives* that table from the shared schema before its own seed overwrites the system rows — a dependency on the shared **seed**, not on this application's **read**. A dependency recorded once, in prose, about another repository, is the shape that keeps work parked after the blocker has moved.

## Decision

**`TSM_AUTHZ_ROLE_SOURCE` is removed, and setting it fails the boot.** `approles.Members` has one role source: this application's `organization_member_roles` joined to its `role_templates`. The `RoleSource` type, `ParseRoleSource`, the per-read `identity` branch and the startup line's rollback hint are gone.

**The tombstone refuses every value, from either source.** `config.Load` returns an error naming the key for any non-empty `authz.role_source`, whether it arrives through the file or through `TSM_AUTHZ_ROLE_SOURCE`. `identity` is refused because the position no longer exists. `app` is refused too: it is now merely the truth, and accepting it would keep alive a setting that chooses nothing, to be copied into the next deployment's config as if it still did. Ignoring the key was the other option and is rejected — this repository has already had a setting that was accepted and not honoured (`normaliseRoleSource`'s history), and a deployment carrying `role_source: identity` as the record of a rollback it does not have is that shape again. The env path is bound explicitly, because viper decodes an environment override only for keys it knows and a retired key with no default is one it would otherwise not.

**A `Members` with no application connection refuses role reads.** Before, it degraded to identity's role columns and announced it. That degradation was the retired position leaking back in through construction — on the unit-test rigs and legacy sites nobody re-reads, and silently. Now every role-carrying read on such a `Members` returns `ErrNoAppStore`, the sentinel handlers already map to 500, and it does so *before* touching identity. Writes on such a `Members` still degrade to the identity leg alone; that is the documented rig shape and it is out of this decision's scope.

**The dual write stays. Nothing is dropped.** `identity.organization_members.role_template_id` and `identity.role_templates` remain, still written by this application, still compared by `CheckDrift` and the `tsm_authz_role_drift` metric. Both go in #206's final phase. This decision removes a *reader*; removing the *writer* is a cross-repository sequence (registry first, then here) and bundling it would have made this change unrevertable by redeploy.

**`TSM_SUITE_ROLE_SEED_OWNER` and `seedSharedRoleTemplates` are untouched.** The setting also gates the setup wizard's identity-ownership decisions, which are about who owns identity rather than about role templates. The seed still has a reader — the registry's startup derivation, above — so removing it here would be a behaviour change in the other application made from this one.

**`authz-drift` keeps its gate role, aimed at a different edge.** It gated the flip; it now gates the removal of the flip's rollback. The release carrying this decision is the last point at which a divergence between the two schemas can be corrected by rolling back rather than by repair, so it is run before upgrading onto that release and required to exit zero.

## Consequences

**Easier**:

- The registry can retire `SeedSharedIdentityRoleTemplates` without silently breaking a rollback in this application, which unblocks #206 Phase 4 in both repositories.
- `approles.NewMembers` has two parameters again. Every construction site stops stating a role source, because there is nothing to state.
- The test rigs that exercise role reads exercise the production read path — identity for membership, this application's tables for the role — instead of the retired one.

**Harder**:

- **Rollback of the read model is a redeploy of the previous image**, not a restart with a setting. The previous build still has the lever and finds the shared schema current, so this is a real rollback; it is simply a slower one.
- A deployment that carried `authz.role_source` explicitly — even as `app` — does not boot until the line is removed. That is deliberate, and it is a breaking change with an upgrade note.
- Test rigs that build a `Members` without an application connection can no longer read roles through it. A rig that needs a role read supplies an application connection and stages the overlay query; a rig that only exercises writes is unchanged.

**Amends [ADR 006](006-per-app-authorization-reads.md)**: its "The rollback is a restart" paragraph and the "Unchanged, for now" section describe the lever and the reasons it kept the shared seed. The lever is gone. Of the two reasons, "the rollback path reads it" is what this decision removes, and "the sibling registry still authorizes from it" is corrected above to what the sibling actually does. ADR 006 otherwise stands.
