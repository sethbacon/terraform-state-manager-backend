# The release-PR closing-keyword guard: what it closes, and what it does not

This documents a **deliberately partial** guard. The gap below is real, is not
closed by anything in this repository, and needs an operator decision. An honest
partial guard with a stated limit is worth more than one that looks total and is
not.

Code: `.github/workflows/release-pr-guard.yml`, `.github/release-pr-closing-keywords/`.

## The defect

Merging a release pull request closes every issue in GitHub's linked-issue graph
for that pull request — `closingIssuesReferences`. Issues land in that graph two
ways:

1. a closing keyword in the body, which release-please emits for *every* issue
   reference a commit carries, including a deliberately non-closing `Refs #N`;
2. **the Development panel**, which writes a `connected` timeline event and no
   body text whatsoever.

The guard reads `closingIssuesReferences`, so it sees both. The problem was never
*what* it reads. It was *when*.

## Why no pre-merge trigger can be complete

`connected` **is not an activity type on any webhook.** GitHub's `issues`
activity types are `assigned`, `closed`, `deleted`, `demilestoned`, `edited`,
`field_added`, `field_removed`, `labeled`, `locked`, `milestoned`, `opened`,
`pinned`, `reopened`, `transferred`, `typed`, `unassigned`, `unlabeled`,
`unlocked`, `unpinned`, `untyped`. `pull_request` has no `connected` either.
Linking an issue through the panel fires **nothing, anywhere**.

Measured on the real incident, release pull request #243:

| time | event | workflow run |
|---|---|---|
| 22:01:36 | last force-push | — |
| 22:01:39 | last `pull_request` runs (CI, PR Checks ×2) | yes |
| 22:02:09 | `connected` — the link is made | **none** |
| 22:11:28 | merge | — |
| 22:11:29 | issue #245 closes | — |

Replaying the guard in the state that held at 22:01:39 prints `linked=0
graded=0` and exits 0.

## What was rejected

- **The `issues` event** — impossible, not merely awkward. No `connected` or
  `linked` activity type exists on it. There is nothing to subscribe to.
- **"A required context re-evaluated rather than cached"** — no such mechanism.
  Branch protection grades the latest check run or status **on the head SHA** and
  never re-evaluates at merge time. A verdict only changes when something posts a
  new one, which is a trigger problem again.
- **`merge_group` (merge queue)** — **the only complete answer, and unavailable
  here.** It grades after dequeue, at merge time, when the answer is final. But
  this repository has no merge queue configured; enabling one is a repository
  setting no pull request can make; and `enforce_admins` is **false** while
  release pull requests here are merged with `--admin`, which bypasses a queue
  outright. It would gate everyone except the actor who actually merges releases.

## What is implemented

**1. `schedule` — bounds the window.** Every 5 minutes, `link-regrade` re-grades
every open pull request against the live link graph and publishes the verdict as
a commit status `release-guard/link-regrade` **on the head SHA**, the only place
protection looks. The pull-request job posts the same context immediately, so no
pull request waits on a status only the cron can produce, and the cron can
overwrite it — the last status posted for a context wins. A 5-minute tick would
have fired twice inside #243's 9m19s gap.

**2. `push` to `main` — grades after the fact and repairs.** `merge-backstop`
re-runs the *same* `evaluate()` with the clock wound back to the merge instant,
and **reopens** any issue the merge closed that no commit in the release asked to
close, with a comment saying why. It cannot prevent the close. It removes the
part that did the damage: that the close was silent.

Verified against live artifacts: it fails #243 (→ #245) and #480 (→ #459), the
two known incidents, and passes releases #490, #494, #498, #506 and #515.

> The post-merge grade reads the **pull request body**, never the merge commit
> message. `squash_merge_commit_message=COMMIT_MESSAGES` means the *branch's*
> commit messages, and a release-please branch has exactly one commit. The real
> merge commits are `chore(main): release 2.6.0 (#243)` and
> `chore(main): release 3.13.0 (#480)` plus a `Co-authored-by` trailer — no
> changelog, no keyword. Both closes came from the link graph alone. A guard
> aimed at the merge commit message would grade an empty universe and report
> clean on both.

## THE RESIDUAL — what an operator still has to do

**R1. Two contexts are not required, so today the guard blocks nothing.**
`main`'s required contexts do not include `Release PR closes only what it
completes`, `Release-PR guard self-test`, or the new
`release-guard/link-regrade`. All three run and report; none gates. **Add
`release-guard/link-regrade` to the required contexts** or the bounded window is
decorative.

**R2. `enforce_admins` is false.** An `--admin` merge bypasses every required
context, whatever it says. Release pull requests in this estate are merged that
way. Until admins are enforced, **item 1 does not bind the person who merges
releases**, and only the post-merge backstop applies.

**R3. One cron tick is still exploitable.** A link made and merged inside the
same 5-minute window merges green. GitHub does not guarantee cron punctuality
and may delay a tick under load, so the true window is ≥5 minutes. The backstop
catches it after the fact; nothing prevents it.

**R4. Scheduled workflows are disabled after 60 days of repository inactivity.**
If that happens, `link-regrade` stops silently and the window reopens to
infinity, looking exactly like a passing guard. It is worth checking that this
workflow has recent scheduled runs when auditing.

**R5. The backstop repairs, it does not prevent.** Between merge and the
`push` run the issue is genuinely closed. Anything watching issue-closed events
in that window sees the wrong state.

**R6. The backstop grades `github.sha` only — the head of the push.** If a
release merge ever landed in the *middle* of a multi-commit push to `main`, the
commits behind the head would not be graded. This is deliberate rather than
overlooked: with squash merges onto a protected branch each merge is its own
push, so the head *is* the merge commit; and the alternatives — walking
`github.event.commits`, which is capped at 20 entries and is empty on a
force-push — add failure modes worth more than the case they cover. If direct or
batched pushes to `main` are ever allowed, this needs revisiting.

### The only way to close R3 completely

Enable a merge queue on `main`, require `release-guard/link-regrade` in it, and
set `enforce_admins: true`. That moves the grade to dequeue time, after which no
link can be added. It also means release pull requests can no longer be merged
with `--admin`, which is a workflow change, not just a settings change — which is
why it is written down here as a decision rather than made silently.
