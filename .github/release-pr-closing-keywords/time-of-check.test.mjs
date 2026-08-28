// The TIME-OF-CHECK axis: the guard beside this file reads the right universe
// at the wrong moment.
//
// `closingIssuesReferences` changes on a `connected` timeline event -- an issue
// attached through the Development panel -- and `connected` is an activity type
// on NO webhook, neither `pull_request` nor `issues`. So a link made after the
// last push fires nothing, and the pull-request-time guard never looks again.
//
// The incident these cases are cut from, re-measured against the live API:
//
//   22:01:36  last force-push to #243's head branch
//   22:01:39  last `pull_request` workflow runs -- CI, PR Checks x2
//   22:02:09  `connected`, and NO workflow run follows it
//   22:11:28  merge          22:11:29  issue #245 closes
//
// Two halves are tested here. The FIRST is the after-the-fact grade, which is
// the only code that can run at a moment nothing can dodge. The SECOND is the
// workflow wiring, because every mechanism in this fix is a trigger, and a
// trigger that is quietly deleted looks exactly like a trigger that never
// fired -- which is this whole cluster's failure mode.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Blind } from './verify.mjs';
import { stateAtMerge, gradeMergedRelease } from './merge-backstop.mjs';

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const U = (n) => `https://github.com/${O}/${R}/issues/${n}`;
const C = (sha) => `https://github.com/${O}/${R}/commit/${sha}`;
const SHA = (p) => (p + '0'.repeat(40)).slice(0, 40);

// The real timestamps. #245 closed ONE SECOND after #243 merged.
const MERGED_AT = '2026-07-23T22:11:28Z';
const CLOSED_AT = '2026-07-23T22:11:29Z';

// #243's body, in the shape the live API returns it: a compare link, one commit
// link, and NOT ONE closing keyword anywhere.
const PR243_BODY =
  ':robot: I have created a release *beep* *boop*\n---\n\n\n' +
  `## [2.6.0](https://github.com/${O}/${R}/compare/v2.5.0...v2.6.0) (2026-07-23)\n\n\n### Bug Fixes\n\n` +
  `* adopt org_owner/org_provisioner scopes ([#246](${U(246)})) ([003d043](${C(SHA('003d043'))}))\n`;

const REFS_TRAILER = 'fix: adopt org_owner scopes\n\nRefs #245. Refs #245.';

const grade = ({ body = PR243_BODY, mergedAt = MERGED_AT, refs, closedAt, message = REFS_TRAILER, hasNextPage = false } = {}) =>
  gradeMergedRelease({
    owner: O,
    repo: R,
    body,
    mergedAt,
    linked: async () => ({ hasNextPage, refs }),
    closedAt: async () => closedAt,
    commitMessage: async () => message,
  });

const ids = (r) => r.results.filter((x) => x.verdict === 'fail').map((x) => x.id);

// -- HALF 1: the clock ------------------------------------------------------

test('TOC: an issue closed one second AFTER the merge was OPEN at the merge', () => {
  assert.equal(stateAtMerge({ closedAt: CLOSED_AT, mergedAt: MERGED_AT }), 'open');
});

test('TOC: an issue closed before the merge was already closed and lost nothing', () => {
  assert.equal(stateAtMerge({ closedAt: '2026-07-20T00:00:00Z', mergedAt: MERGED_AT }), 'closed');
});

test('TOC: closed at the exact merge instant counts as open, because the merge is what closed it', () => {
  assert.equal(stateAtMerge({ closedAt: MERGED_AT, mergedAt: MERGED_AT }), 'open');
});

test('TOC: an issue that was never closed is open', () => {
  assert.equal(stateAtMerge({ closedAt: null, mergedAt: MERGED_AT }), 'open');
});

test('TOC: an unusable merge timestamp is refused, not treated as "closed long ago"', () => {
  assert.throws(() => stateAtMerge({ closedAt: CLOSED_AT, mergedAt: undefined }), Blind);
  assert.throws(() => stateAtMerge({ closedAt: CLOSED_AT, mergedAt: 'whenever' }), Blind);
});

test('TOC: an unusable closed_at is refused rather than assumed safe', () => {
  assert.throws(() => stateAtMerge({ closedAt: 'lunchtime', mergedAt: MERGED_AT }), Blind);
});

// -- HALF 1b: the incident, end to end --------------------------------------

test('TOC: the #243 merge is FAILED after the fact, from a body with no keyword at all', async () => {
  const r = await grade({ refs: [{ owner: O, repo: R, issue: 245 }], closedAt: CLOSED_AT });
  assert.deepEqual(ids(r), [`${O}/${R}#245`]);
  assert.equal(r.results[0].source, 'github-linked');
});

// THE regression case. `evaluate()` short-circuits to the state carried on the
// ref when there is one, and after the merge GitHub reports every issue the
// merge closed as CLOSED -- which grades as "already closed, cannot lose
// anything" and clears the violation. The backstop must DROP that state and
// consult the clock instead. Hand `state: 'CLOSED'` in and it must still fail.
test('TOC: a CLOSED state on the linked ref does not launder the violation', async () => {
  const r = await grade({
    refs: [{ owner: O, repo: R, issue: 245, state: 'CLOSED' }],
    closedAt: CLOSED_AT,
  });
  assert.deepEqual(ids(r), [`${O}/${R}#245`], 'the post-merge state was trusted and the incident vanished');
});

test('TOC: a release whose issue was genuinely closed beforehand stays clean', async () => {
  const r = await grade({
    refs: [{ owner: O, repo: R, issue: 245, state: 'CLOSED' }],
    closedAt: '2026-07-01T00:00:00Z',
  });
  assert.deepEqual(ids(r), []);
});

test('TOC: a release whose commit really does close the issue stays clean', async () => {
  const r = await grade({
    refs: [{ owner: O, repo: R, issue: 245 }],
    closedAt: CLOSED_AT,
    message: 'fix: adopt org_owner scopes\n\nCloses #245',
  });
  assert.deepEqual(ids(r), []);
});

test('TOC: a truncated authoritative set is refused after the merge too', async () => {
  await assert.rejects(
    () => grade({ refs: [{ owner: O, repo: R, issue: 245 }], closedAt: CLOSED_AT, hasNextPage: true }),
    Blind
  );
});

test('TOC: a reader returning no refs array is refused rather than read as nothing to do', async () => {
  await assert.rejects(
    () =>
      gradeMergedRelease({
        owner: O,
        repo: R,
        body: PR243_BODY,
        mergedAt: MERGED_AT,
        linked: async () => ({ hasNextPage: false }),
        closedAt: async () => null,
        commitMessage: async () => REFS_TRAILER,
      }),
    Blind
  );
});

// -- HALF 2: the wiring -----------------------------------------------------
//
// Every mechanism above is reachable only if the workflow still declares the
// triggers and permissions that reach it. These cases read the workflow file
// itself, because a deleted `schedule:` block and a `schedule:` block that
// never fired produce identical evidence: nothing.

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WF = fs.readFileSync(path.join(HERE, '..', 'workflows', 'release-pr-guard.yml'), 'utf8');
// The re-grade MECHANISM lives in link-regrade.sh so link-regrade.test.mjs can
// EXECUTE it against a stub gh -- outcomes are asserted there. The pins below
// against the script's text are secondary: they catch a rename or a rewiring
// of the workflow around the script, which the harness cannot see.
const REGRADE_SH = fs.readFileSync(path.join(HERE, 'link-regrade.sh'), 'utf8');

// The job bodies, split on a two-space-indented job key at column 0 of the
// jobs map. Used so a permission can be asserted against the RIGHT job.
function jobBlocks(text) {
  const jobsAt = text.indexOf('\njobs:\n');
  assert.ok(jobsAt > 0, 'no jobs: map in the workflow');
  const body = text.slice(jobsAt + 7);
  const out = new Map();
  const re = /^ {2}([a-z][a-z0-9-]*):$/gm;
  const starts = [...body.matchAll(re)];
  starts.forEach((m, i) => {
    const end = i + 1 < starts.length ? starts[i + 1].index : body.length;
    out.set(m[1], body.slice(m.index, end));
  });
  return out;
}

const JOBS = jobBlocks(WF);

test('WIRING: the workflow enumerates jobs at all, and the set is not empty', () => {
  assert.ok(JOBS.size > 0, 'parsed zero jobs -- the parser broke, not the workflow');
  assert.ok(JOBS.size >= 4, `expected at least 4 jobs, parsed ${JOBS.size}: ${[...JOBS.keys()]}`);
});

test('WIRING: a schedule trigger exists and ticks at least every 15 minutes', () => {
  const m = WF.match(/^\s*- cron: "([^"]+)"/m);
  assert.ok(m, 'no cron: the time-of-check window is unbounded again');
  const minute = m[1].split(/\s+/)[0];
  const step = /^\*\/(\d+)$/.exec(minute);
  assert.ok(step, `cron minute field ${minute} is not a */N step, so the window is not bounded`);
  assert.ok(Number(step[1]) <= 15, `a ${step[1]}-minute tick is wider than the 9m19s gap the incident used`);
});

test('WIRING: a push trigger on main exists, so the after-the-fact grade runs', () => {
  assert.match(WF, /\n {2}push:\n {4}branches: \[main\]/, 'no push trigger: nothing grades the merge');
});

test('WIRING: the merge backstop is on push, may write issues, and invokes the backstop', () => {
  const j = JOBS.get('merge-backstop');
  assert.ok(j, 'the merge-backstop job is gone');
  assert.match(j, /if: github\.event_name == 'push'/);
  assert.match(j, /issues: write/, 'without issues:write it cannot reopen, which is the repair');
  assert.match(j, /merge-backstop\.mjs/);
});

test('WIRING: the scheduled re-grade job invokes the extracted script, which runs the SAME verifier', () => {
  const j = JOBS.get('link-regrade');
  assert.ok(j, 'the link-regrade job is gone: the window is unbounded again');
  assert.match(
    j,
    /bash \.github\/release-pr-closing-keywords\/link-regrade\.sh/,
    'the job no longer runs link-regrade.sh, so everything the harness proves about the script proves nothing about CI'
  );
  assert.match(REGRADE_SH, /verify\.mjs/, 're-grading with different code than the PR job is a second guard to drift');
  assert.match(j, /statuses: write/);
});

// Branch protection grades a CONTEXT ON THE HEAD SHA. If the two jobs post
// under different names, the cron can never overwrite the pull-request-time
// pass, and the whole bounded-window mechanism is decorative.
test('WIRING: both publishers post under ONE context name, and it is enumerated', () => {
  const declared = WF.match(/^ {2}REGRADE_CONTEXT: (\S+)$/m);
  assert.ok(declared, 'no REGRADE_CONTEXT declared');

  const bindings = [...WF.matchAll(/^\s*CONTEXT: \$\{\{ env\.REGRADE_CONTEXT \}\}$/gm)];
  assert.ok(bindings.length > 0, 'enumerated zero CONTEXT bindings -- the matcher is blind, not the file clean');
  assert.equal(bindings.length, 2, `expected the PR job and the re-grade to bind CONTEXT, found ${bindings.length}`);

  assert.match(
    JOBS.get('closing-keywords'),
    /-X POST "repos\/\$REPO\/statuses\/\$/,
    'closing-keywords never posts a commit status, so its verdict reaches nothing protection reads'
  );
  assert.match(
    REGRADE_SH,
    /-X POST "repos\/\$REPO\/statuses\/\$/,
    'the re-grade script never posts a commit status, so the cron overwrites nothing'
  );
  assert.match(
    JOBS.get('link-regrade'),
    /CONTEXT: \$\{\{ env\.REGRADE_CONTEXT \}\}/,
    'the re-grade job does not hand the script the shared context name'
  );
});

// GitHub caps statuses at 1000 per SHA per context. A 5-minute tick that
// re-posts an unchanged verdict reaches that in about three and a half days,
// after which a long-lived pull request silently stops being gradeable -- a
// guard that expires by running normally.
test('WIRING: the re-grade script reads the current status, paginated, before posting', () => {
  // The unchanged-verdict skip itself is EXECUTED and asserted in
  // link-regrade.test.mjs; these pins only catch the mechanism moving.
  assert.match(REGRADE_SH, /commits\/\$_sha\/status\?per_page=100/, 'never reads the existing status, so it re-posts every tick');
  assert.match(REGRADE_SH, /first\(/, 'should select with jq first, not a pipe a closed reader can empty');
  assert.doesNotMatch(REGRADE_SH, /statuses\[\][^\n]*\|\s*head/, 'piping into head can truncate to a silent empty answer');
});

test('WIRING: the PR job publishes a status and still fails on a failed grade', () => {
  const j = JOBS.get('closing-keywords');
  assert.match(j, /statuses: write/);
  assert.match(j, /continue-on-error: true/);
  // continue-on-error rewrites the step CONCLUSION to success. If nothing
  // re-asserts `outcome`, a failing grade becomes a green required check --
  // the exact vacuous pass this file exists to remove.
  assert.match(j, /if: steps\.grade\.outcome != 'success'/, 'a failed grade would report green');
});

// A scheduled tick sharing a concurrency group with the post-merge backstop
// would CANCEL it, and the backstop is the one job that cannot be re-run by
// pushing again.
test('WIRING: the concurrency key separates events, so a tick cannot cancel the backstop', () => {
  const m = WF.match(/^ {2}group: (.+)$/m);
  assert.ok(m, 'no concurrency group');
  assert.match(m[1], /github\.event_name/, 'every non-pull_request event collapses to one group and cancels');
  const cancel = WF.match(/^ {2}cancel-in-progress: (.+)$/m);
  assert.ok(cancel, 'no cancel-in-progress');
  assert.notEqual(cancel[1].trim(), 'true', 'unconditional cancellation lets a tick kill the backstop');
});

// The hardening gate in this repository requires these of every job. Asserting
// it here means the guard's own jobs cannot be the ones that regress it.
test('WIRING: every job is bounded by a timeout and hardens the runner first', () => {
  const checked = [];
  for (const [name, body] of JOBS) {
    assert.match(body, /timeout-minutes: \d+/, `${name} has no timeout`);
    assert.match(body, /step-security\/harden-runner@[0-9a-f]{40} # v/, `${name} lacks a pinned harden-runner`);
    checked.push(name);
  }
  assert.ok(checked.length >= 4, `enumerated only ${checked.length} job(s); the floor is the point`);
});
