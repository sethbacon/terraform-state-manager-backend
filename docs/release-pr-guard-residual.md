# The release-PR closing-keyword guard: what it closes, and what it does not

This repository now runs the shared guard published from
`4cloudguru/shared-workflows` (`.github/actions/release-pr-closing-keywords`),
called from `.github/workflows/release-pr-guard.yml`. The defect, the rejected
alternatives, and the general residual (R1–R8) are documented once, centrally,
so they do not drift out of sync across every repository that adopts the guard:

- what it defends against, and why no pre-merge trigger can be complete:
  `docs/release-pr-guard-adoption.md` and `docs/release-pr-guard-residual.md` in
  `4cloudguru/shared-workflows`.
- the workflow this repository's `release-pr-guard.yml` copies from, and the
  inputs a consumer may need to override: the same adoption doc.

This file keeps only what is specific to **this** repository: its own required
contexts, its own `enforce_admins` decision, and the fixed incident record the
guard was built from.

## What fired here before the guard existed

Two real closes, both against `sethbacon/terraform-state-manager-backend`:

- release pull request **#243 → issue #245**, 2026-07-23. #243's body carried
  not one closing keyword; #245 was attached through the Development panel,
  which writes a `connected` timeline event and no body text at all, and closed
  one second after the merge. This is the incident that proved no pre-merge
  webhook can be complete (see the shared residual doc's "why no pre-merge
  trigger can be complete") and the one the local test suite this migration
  removed was built to reproduce.
- release pull request **#480 → issue #459**, 2026-08-25. Commit `ca2e5b3` ended
  a deliberately non-closing `Refs #459`; release-please rendered it as a
  closing keyword in the changelog body regardless.

Both are now covered by the shared action's own test suite
(`4cloudguru/shared-workflows` `.github/actions/release-pr-closing-keywords/`,
plus `tests/test-release-pr-closing-keywords.js`), which is byte-identical to
this repository's former local fixtures for the #243/#245 case and adds
coverage this repository's local suite did not have (119 cases there against
107 here at the time of the port).

## THE RESIDUAL — what is specific to this repository

**Required contexts, re-derived 2026-09-07:** this repository's `main` carries
eleven required status checks, including `Release PR closes only what it
completes` (the `closing-keywords` job, still posted under that exact name by
the shared guard) and `release-guard/link-regrade`. Verify current state, don't
trust this file:

```
gh api repos/sethbacon/terraform-state-manager-backend/branches/main/protection/required_status_checks --jq .contexts
```

**`Release-PR guard self-test` was a required context with nothing left to post
it; it has since been removed from the required list.** That job ran
`.github/release-pr-closing-keywords/`'s own `node --test` suite; the migration
to the shared action deleted that directory, and `release-pr-guard.yml` no
longer defines a job by that name — see the comment block at the bottom of that
file for why an equivalent local job was not kept (the suite it would run now
lives in shared-workflows' own CI, gating shared-workflows' own `main`, not this
repository's). While it stayed listed, that context was required and permanently
unreported — indistinguishable, at the API level, from a context nobody ever
added, but present in branch protection's list. It is absent from the list
re-derived above. This paragraph is kept, rather than deleted, because a
required context that nothing posts blocks every non-admin merge forever, and
that is the failure shape to watch for whenever a job is renamed or removed.

**`release-guard/link-regrade` is now a required context here.** The commit
status the scheduled re-grade overwrites is in this repository's required list,
so the bounded time-of-check window is load-bearing at merge time rather than
decorative: a link attached through the Development panel after the last
`pull_request` event — the exact #243/#245 shape recorded above, which writes a
`connected` timeline event and emits no activity a pre-merge trigger can see —
is caught by the re-grade before a non-admin merge can proceed.

**`enforce_admins` is `false` — by deliberate decision recorded on issue #529,
not an oversight.** An `--admin` merge bypasses every required context, and
release pull requests here are merged that way. So no required context in this
repository binds the person who merges releases; only the post-merge backstop
(`merge-backstop`, `push` to `main`) applies to them. The recorded plan is to
flip `enforce_admins` to `true` once this project gains a second reviewer, at
which point every mechanism in the shared guard engages here with no further
work. Verify the current state, don't assume it hasn't changed:

```
gh api repos/sethbacon/terraform-state-manager-backend/branches/main/protection/enforce_admins --jq .enabled
```
