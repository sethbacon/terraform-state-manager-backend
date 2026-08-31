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

**Required contexts, re-derived at the time of this change (2026-08-30):** this
repository's `main` carries eleven required status checks, including both
`Release PR closes only what it completes` (the `closing-keywords` job, still
posted under that exact name by the shared guard) and `Release-PR guard
self-test`. Verify current state, don't trust this file:

```
gh api repos/sethbacon/terraform-state-manager-backend/branches/main/protection/required_status_checks --jq .contexts
```

**`Release-PR guard self-test` is now a required context with nothing left to
post it.** That job ran `.github/release-pr-closing-keywords/`'s own
`node --test` suite; this migration deleted that directory, and
`release-pr-guard.yml` no longer defines a job by that name — see the comment
block at the bottom of that file for why an equivalent local job was not kept
(the suite it would run now lives in shared-workflows' own CI, gating shared-
workflows' own `main`, not this repository's). Removing `Release-PR guard
self-test` from this repository's required status checks is a branch-protection
setting this pull request cannot make. Until an admin does, that context is
required and permanently unreported — indistinguishable, at the API level, from
a context nobody ever added, but present in branch protection's list, and
merges here already rely on `--admin` (see R2 below) to get past exactly that
shape of thing.

**`release-guard/link-regrade` — still not a required context here,** matching
the shared doc's residual: the commit status the cron overwrites is not in this
repository's required list. Until it is, the bounded time-of-check window is
decorative at merge time.

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
