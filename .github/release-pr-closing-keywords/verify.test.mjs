// Cases, not coverage. Each one is a spelling that either did slip past a
// regex somewhere in this estate or is a spelling GitHub acts on that a naive
// `#\d+` matcher cannot see.
import test from 'node:test';
import assert from 'node:assert/strict';
import { findClosingReferences, key } from './closing-refs.mjs';
import { evaluate, Blind } from './verify.mjs';

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const U = (n) => `https://github.com/${O}/${R}/issues/${n}`;
const C = (sha) => `https://github.com/${O}/${R}/commit/${sha}`;
// 40 hex characters exactly, the way a changelog link spells one.
const SHA = (p) => (p + '0'.repeat(40)).slice(0, 40);
const A = SHA('aaaaaaa');

const HEADER = `## [3.16.0](https://github.com/${O}/${R}/compare/v3.15.0...v3.16.0) (2026-08-27)\n\n### Features\n\n`;

const run = (body, { states = {}, messages = {} } = {}) =>
  evaluate({
    body,
    owner: O,
    repo: R,
    issueState: async (ref) => states[key(ref)] ?? 'closed',
    commitMessage: async (sha) => messages[sha] ?? 'chore: something with no trailer',
  });

const ids = (r) => r.results.filter((x) => x.verdict === 'fail').map((x) => x.id);

// -- what release-please actually emits ------------------------------------

test('the markdown-link spelling release-please emits is seen at all', () => {
  const { closing } = findClosingReferences(`, closes [#393](${U(393)})`, O, R);
  assert.equal(closing.length, 1);
  assert.equal(closing[0].issue, 393);
});

test('a Refs-only tracking issue that is still open fails the release', async () => {
  const body = `${HEADER}* **tenancy:** scope the schedules reads ([#519](${U(519)})) ([d6b568c](${C('d6b568ce672288c0123f13c9e7ce8b05eee0a728')})), closes [#393](${U(393)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#393`]: 'open' },
    messages: { d6b568ce672288c0123f13c9e7ce8b05eee0a728: 'feat(tenancy): scope\n\nRefs #393' },
  });
  assert.deepEqual(ids(r), [`${O}/${R}#393`]);
});

test('the same line passes once a commit in the release really closes it', async () => {
  const body = `${HEADER}* **tenancy:** scope ([d6b568c](${C('d6b568ce672288c0123f13c9e7ce8b05eee0a728')})), closes [#393](${U(393)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#393`]: 'open' },
    messages: { d6b568ce672288c0123f13c9e7ce8b05eee0a728: 'feat(tenancy): scope\n\nCloses #393' },
  });
  assert.deepEqual(ids(r), []);
});

test('an already-closed issue is harmless and does not block the release', async () => {
  const body = `${HEADER}* **audit:** holds ([74095a9](${C('74095a9c55008fe9eccf3b7df666748cebac04de')})), closes [#373](${U(373)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#373`]: 'closed' },
    messages: { '74095a9c55008fe9eccf3b7df666748cebac04de': 'feat(audit): holds\n\nRefs #373' },
  });
  assert.deepEqual(ids(r), []);
});

test('a commit closing one issue does not license closing a different one', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#2](${U(2)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#2`]: 'open' },
    messages: { [A]: 'feat: y\n\nCloses #1' },
  });
  assert.deepEqual(ids(r), [`${O}/${R}#2`]);
});

// -- every other spelling GitHub acts on ------------------------------------

for (const [label, text, want] of [
  ['bare hash', 'closes #12', 12],
  ['GH- form', 'Fixes GH-12', 12],
  ['bare URL', `resolved ${U(12)}`, 12],
  ['colon', 'Closed: #12', 12],
  ['uppercase', 'RESOLVES #12', 12],
  ['newline between', 'Closes\n#12', 12],
  ['link text and href disagree; link TEXT is what GitHub reads', `closes [#12](${U(999)})`, 12],
]) {
  test(`sees the ${label} spelling`, () => {
    const { closing } = findClosingReferences(text, O, R);
    assert.equal(closing.length, 1, `${label} was invisible`);
    assert.equal(closing[0].issue, want);
  });
}

test('a cross-repository reference keeps its own owner and repo', () => {
  const { closing } = findClosingReferences('fixes other/thing#7', O, R);
  assert.equal(key(closing[0]), 'other/thing#7');
});

// -- what must NOT be seen, or the guard blocks every release ---------------

for (const [label, text] of [
  ['a Refs trailer', 'Refs #12'],
  ['a bare reference', 'see #12'],
  ['"discloses", which ends in "closes"', 'discloses #12'],
  ['"prefixed", which ends in "fixed"', 'prefixed #12'],
  ['a Bug Fixes heading above an entry', `### Bug Fixes\n\n* **a:** b ([#12](${U(12)}))`],
  ['a fix type prefix', `* **fix:** something ([#12](${U(12)}))`],
  ['the word resolve used as prose', 'resolve the drift ([#12](' + U(12) + '))'],
]) {
  test(`does not treat ${label} as closing`, () => {
    assert.deepEqual(findClosingReferences(text, O, R).closing, []);
  });
}

// -- only the first reference in a run closes -------------------------------

test('references behind a spent keyword are reported, not failed', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#1](${U(1)}) [#2](${U(2)})\n`;
  const r = await run(body, { states: { [`${O}/${R}#1`]: 'closed', [`${O}/${R}#2`]: 'open' } });
  assert.deepEqual(ids(r), []);
  assert.deepEqual(r.trailing.map(key), [`${O}/${R}#2`]);
});

test('a second keyword starts a new run and does close again', () => {
  const { closing } = findClosingReferences('closes #1 and fixes #2', O, R);
  assert.deepEqual(closing.map((c) => c.issue), [1, 2]);
});

// -- the floors: a guard that cannot see must fail, not pass ----------------

test('an unrecognisable body is refused rather than reported clean', async () => {
  await assert.rejects(() => run('just some words, closes #1'), Blind);
});

test('an empty body is refused', async () => {
  await assert.rejects(() => run(''), Blind);
});

test('closing references with an empty commit universe are refused', async () => {
  await assert.rejects(() => run(`${HEADER}* **x:** y, closes [#1](${U(1)})\n`), Blind);
});

test('an empty commit message is a failed lookup, not a commit without trailers', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#1](${U(1)})\n`;
  await assert.rejects(
    () => run(body, { messages: { [A]: '   ' } }),
    Blind
  );
});

test('an unknown issue state is refused rather than assumed safe', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#1](${U(1)})\n`;
  await assert.rejects(() => run(body, { states: { [`${O}/${R}#1`]: 'merged' } }), Blind);
});

test('a release with no closing references and no commits is still recognised', async () => {
  const r = await run(`${HEADER}nothing here\n`);
  assert.deepEqual(r.results, []);
});
