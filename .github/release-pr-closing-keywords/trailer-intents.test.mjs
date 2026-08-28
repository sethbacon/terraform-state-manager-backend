// Closing INTENT is read from trailer-position lines only, with git as the
// parser. These cases exist because the free-text scan before them was
// FLIPPED by quotation: three spellings, each carrying the words "closes #245"
// in text that is not the author asking for anything, turned the exact #243
// incident from FAIL to PASS. Each is re-run here end-to-end through
// evaluate(), where the baseline must FAIL and all three spellings must now
// ALSO FAIL -- a revert is by definition not completing the issue.
import test from 'node:test';
import assert from 'node:assert/strict';
import { closingIntentsFromTrailers } from './trailer-intents.mjs';
import { evaluate } from './verify.mjs';

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const U = (n) => `https://github.com/${O}/${R}/issues/${n}`;
const C = (sha) => `https://github.com/${O}/${R}/commit/${sha}`;
const SHA = (p) => (p + '0'.repeat(40)).slice(0, 40);

const intents = (msg) => closingIntentsFromTrailers(msg, O, R).map((r) => `${r.owner}/${r.repo}#${r.issue}`);

// -- trailer spellings that ARE the author's ask -----------------------------

for (const [label, message, want] of [
  ['the estate convention, no colon', 'fix: x\n\nCloses #245', [`${O}/${R}#245`]],
  ['the git convention, with colon', 'fix: x\n\nCloses: #245', [`${O}/${R}#245`]],
  ['fixes, resolves and tense variants', 'fix: x\n\nFixed #245', [`${O}/${R}#245`]],
  ['uppercase token', 'fix: x\n\nCLOSES #245', [`${O}/${R}#245`]],
  ['a multi-reference trailer asks for each', 'fix: x\n\nCloses: #245, #246', [`${O}/${R}#245`, `${O}/${R}#246`]],
  ['a cross-repository trailer keeps its coordinates', 'fix: x\n\nCloses: other/thing#7', ['other/thing#7']],
  ['a trailer among other trailers', 'fix: x\n\nCloses #245\nSigned-off-by: A B <a@b.c>', [`${O}/${R}#245`]],
  // git canonicalises `Closes #245` and `Closes: 245` to the same parse, so
  // the bare number is accepted -- in the value slot of a closing trailer it
  // has no other honest reading. Documented in trailer-intents.mjs.
  ['a bare number after the colon', 'fix: x\n\nCloses: 245', [`${O}/${R}#245`]],
  ['prose after a full stop is not part of the ask', 'fix: x\n\nCloses #245. Refs #300.', [`${O}/${R}#245`]],
]) {
  test(`TRAILER: ${label}`, () => {
    assert.deepEqual(intents(message), want);
  });
}

// -- text that must never mint intent ----------------------------------------

for (const [label, message] of [
  // The three proven flips, verbatim shapes.
  ['a git-revert subject quoting a keyword',
    'Revert "fix: adopt org_owner scopes (closes #245)"\n\nThis reverts commit 1111111111111111111111111111111111111111.'],
  ['a fenced code block quoting a keyword',
    'fix: x\n\nThe reporter suggested:\n\n```\ncloses #245\n```\n\nNot done yet.'],
  ['quoted review text',
    'fix: x\n\n> closes #245\n> -- review comment, quoting the eventual goal'],
  // And the shapes that sit one step away from each.
  ['a keyword in the subject line', 'fix: closes #245 properly\n\nSome body text.'],
  ['a keyword in mid-body prose', 'fix: x\n\nThis closes #245 at last.\nMore prose.'],
  ['a subject-only message', 'Closes #245'],
  ['a Refs trailer', 'fix: x\n\nRefs #245. Refs #245.'],
  ['a trailer followed by prose in its paragraph', 'fix: x\n\nCloses #245\nand more prose here'],
  ['a keyword with no reference at all', 'fix: x\n\nCloses: nothing yet'],
]) {
  test(`TRAILER: ${label} is not intent`, () => {
    assert.deepEqual(intents(message), []);
  });
}

// -- the three flips, end to end through evaluate() --------------------------
//
// The #243 shape: a Development-panel link to open #245, a body with no
// keyword, one commit. The verdict must be FAIL for the baseline AND for
// every quoting spelling; before this fix, all three graded PASS.

const PR243_BODY =
  ':robot: I have created a release *beep* *boop*\n---\n\n\n' +
  `## [2.6.0](https://github.com/${O}/${R}/compare/v2.5.0...v2.6.0) (2026-07-23)\n\n\n### Bug Fixes\n\n` +
  `* adopt org_owner/org_provisioner scopes ([#246](${U(246)})) ([003d043](${C(SHA('003d043'))}))\n`;

const gradeIncident = (message) =>
  evaluate({
    body: PR243_BODY,
    owner: O,
    repo: R,
    issueState: async () => 'open',
    commitMessage: async () => message,
    linkedIssues: async () => ({ hasNextPage: false, refs: [{ owner: O, repo: R, issue: 245, state: 'OPEN' }] }),
  });

const failedIds = (r) => r.results.filter((x) => x.verdict === 'fail').map((x) => x.id);

test('FLIP baseline: the Refs-trailer incident still fails', async () => {
  const r = await gradeIncident('fix: adopt org_owner scopes\n\nRefs #245. Refs #245.');
  assert.deepEqual(failedIds(r), [`${O}/${R}#245`]);
});

test('FLIP 1: a revert quoting "closes #245" in its subject fails -- a revert completes nothing', async () => {
  const r = await gradeIncident(
    'Revert "fix: adopt org_owner scopes (closes #245)"\n\nThis reverts commit 1111111111111111111111111111111111111111.'
  );
  assert.deepEqual(failedIds(r), [`${O}/${R}#245`]);
});

test('FLIP 2: a fenced code block quoting "closes #245" fails', async () => {
  const r = await gradeIncident('fix: adopt org_owner scopes\n\nSuggested:\n\n```\ncloses #245\n```\n\nNot done yet.');
  assert.deepEqual(failedIds(r), [`${O}/${R}#245`]);
});

test('FLIP 3: quoted review text saying "closes #245" fails', async () => {
  const r = await gradeIncident('fix: adopt org_owner scopes\n\n> closes #245\n> -- review comment');
  assert.deepEqual(failedIds(r), [`${O}/${R}#245`]);
});

test('FLIP control: a real Closes trailer on the same shape still passes', async () => {
  const r = await gradeIncident('fix: adopt org_owner scopes\n\nCloses #245');
  assert.deepEqual(failedIds(r), []);
});
