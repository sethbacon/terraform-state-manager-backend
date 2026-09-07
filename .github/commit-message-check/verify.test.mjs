// The gate that guards the squash had no test of its own, which is the shape it
// exists to prevent: a check that is wrong reports success it has not earned.
//
// verify.mjs is driven here the way pr-checks.yml drives it -- as a SUBPROCESS,
// reading commits.ndjson and writing GITHUB_STEP_SUMMARY -- so the thing under
// test is the entry point CI runs, not a re-implementation of it.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const VERIFY = new URL('./verify.mjs', import.meta.url).pathname;

/** Run the gate over `messages`, one entry per commit, and return its result. */
function run(messages, { prTitle = 'fix(api): a title', prNumber = '42' } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cmc-'));
  const ndjson = path.join(dir, 'commits.ndjson');
  const summary = path.join(dir, 'summary.md');
  fs.writeFileSync(
    ndjson,
    messages.map(message => JSON.stringify({ sha: 'x', message, parents: 1 })).join('\n') + '\n',
  );
  fs.writeFileSync(summary, '');
  const proc = spawnSync(process.execPath, [VERIFY, ndjson], {
    encoding: 'utf8',
    env: {
      ...process.env,
      PR_TITLE: prTitle,
      PR_NUMBER: prNumber,
      GITHUB_STEP_SUMMARY: summary,
    },
  });
  return {
    code: proc.status,
    stdout: proc.stdout ?? '',
    summary: fs.readFileSync(summary, 'utf8'),
  };
}

test('a well-formed commit passes', () => {
  const r = run(['fix(api): bind the source read to the caller organization']);
  assert.equal(r.code, 0, r.stdout + r.summary);
});

test('a body line release-please cannot parse fails, and names the line', () => {
  // A line STARTING with `name(` reads as a structured token, so its brackets
  // have to be flat and closed. This is the defect the gate was built for.
  const r = run([
    'fix(repositories): scope the list read\n\nWithArgs(pq.Array([]string{"a"})) is what the test now expects.',
  ]);
  assert.equal(r.code, 1);
  assert.match(r.summary, /release-please cannot read/);
});

// ---------------------------------------------------------------------------
// A non-closing reference in trailer position (issue #529, hole 3)
// ---------------------------------------------------------------------------
//
// conventional-changelog's commit partial hardcodes the word `closes` for every
// reference it extracts, so a deliberately non-closing `Refs #N` trailer is
// rendered as `closes [#N]` in the generated release body -- and it comes back
// on every regeneration, because it is baked into a commit on `main`. Release PR
// #515 had to be hand-patched twice.
//
// The remedy is to keep the reference out of the commit in the first place: a
// mention belongs in prose, where it is not extracted as a reference.

test('a Refs trailer is rejected -- it renders as a closing keyword', () => {
  const r = run(['fix(api): narrow the read\n\nRefs #393']);
  assert.equal(r.code, 1, 'a non-closing reference trailer must not reach main');
  assert.match(r.summary, /renders as a closing keyword/);
  assert.match(r.summary, /Refs #393/);
});

test('the two commits that actually caused this are both rejected', () => {
  // The real trailing lines of ca2e5b3 (Refs #459) and d6b568c (Refs #393).
  for (const [message, ref] of [
    ['fix(repositories): leave the statesync list alone\n\nA blanket fixture edit broke it once.\n\nRefs #459', '#459'],
    ['feat(tenancy): flip the drift reads\n\nRepositories package coverage 79.2 percent.\n\nRefs #393', '#393'],
  ]) {
    const r = run([message]);
    assert.equal(r.code, 1, `expected rejection for ${ref}`);
  }
});

test('other non-closing verbs are rejected too, not just Refs', () => {
  for (const trailer of ['See #12', 'Part of #12', 'Related to #12', 'Follow-up to #12']) {
    const r = run([`fix(api): a change\n\n${trailer}`]);
    assert.equal(r.code, 1, `expected rejection for: ${trailer}`);
  }
});

test('a DELIBERATE closing keyword is allowed -- this is not a ban on references', () => {
  for (const trailer of ['Closes #12', 'Fixes #12', 'Resolves #12', 'closes #12']) {
    const r = run([`fix(api): a change\n\n${trailer}`]);
    assert.equal(r.code, 0, `a real close must still pass: ${trailer}`);
  }
});

test('a reference in PROSE is untouched -- that is the remedy the gate suggests', () => {
  const r = run([
    'fix(api): narrow the read\n\nThis builds on the partition work tracked in #393, which is where the\ncolumn came from.',
  ]);
  assert.equal(r.code, 0, r.summary);
});

test('a prose line that ENDS on a reference is still prose, not a trailer', () => {
  // Pins the leading-word limit. Without it the trailer shape swallows ordinary
  // sentences that happen to break the line right after the issue number --
  // found by mutating the limit and watching nothing fail.
  const r = run([
    'fix(api): narrow the read\n\nThe column came from the partition work in #393\nand the flip followed it.',
  ]);
  assert.equal(r.code, 0, r.summary);
});

test('a version-shaped or issue-shaped token that is not a reference does not trip it', () => {
  const r = run(['fix(deps): bump golang from 1.26.7-alpine to 1.26.8-alpine']);
  assert.equal(r.code, 0, r.summary);
});
