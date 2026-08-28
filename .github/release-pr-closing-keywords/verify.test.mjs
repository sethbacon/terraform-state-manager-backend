// Cases, not coverage. Each one is a spelling that either did slip past a
// regex somewhere in this estate, or is a spelling GitHub acts on that a naive
// `#\d+` matcher cannot see, or is a shape the BODY cannot express at all.
import test from 'node:test';
import assert from 'node:assert/strict';
import { findClosingReferences, key } from './closing-refs.mjs';
import { evaluate, Blind, parseLinked } from './verify.mjs';

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const U = (n) => `https://github.com/${O}/${R}/issues/${n}`;
const C = (sha) => `https://github.com/${O}/${R}/commit/${sha}`;
// 40 hex characters exactly, the way a changelog link spells one.
const SHA = (p) => (p + '0'.repeat(40)).slice(0, 40);
const A = SHA('aaaaaaa');

const HEADER = `## [3.16.0](https://github.com/${O}/${R}/compare/v3.15.0...v3.16.0) (2026-08-27)\n\n### Features\n\n`;

// `linked` is what GitHub's closingIssuesReferences would answer: the
// authoritative set. It defaults to empty so every pre-existing case keeps
// grading exactly the body, and the cases that exercise the real universe say
// so explicitly.
const run = (body, { states = {}, messages = {}, linked = { refs: [], hasNextPage: false } } = {}) =>
  evaluate({
    body,
    owner: O,
    repo: R,
    issueState: async (ref) => states[key(ref)] ?? 'closed',
    commitMessage: async (sha) => messages[sha] ?? 'chore: something with no trailer',
    linkedIssues: typeof linked === 'function' ? linked : async () => linked,
  });

const ids = (r) => r.results.filter((x) => x.verdict === 'fail').map((x) => x.id);
const L = (n, state) => ({ owner: O, repo: R, issue: n, state });

// -- AXIS 1: the universe is GitHub's, not the body's -----------------------
//
// Release pull request #243 in this repository is the artifact. Its body is
// 1205 bytes and carries NOT ONE closing keyword; it merged at
// 2026-07-23T22:11:28Z; issue #245 closed at 22:11:29Z. The link was made
// through the Development panel, which writes a `connected` timeline event and
// nothing into the body. The commit, 003d043, says `Refs #245` twice.

const PR243_BODY =
  ':robot: I have created a release *beep* *boop*\n---\n\n\n' +
  `## [2.6.0](https://github.com/${O}/${R}/compare/v2.5.0...v2.6.0) (2026-07-23)\n\n\n### Bug Fixes\n\n` +
  `* adopt org_owner/org_provisioner scopes ([#246](${U(246)})) ([003d043](${C(SHA('003d043'))}))\n`;

test('AXIS 1: a Development-panel link closes an open issue no body text mentions', async () => {
  const r = await run(PR243_BODY, {
    states: { [`${O}/${R}#245`]: 'open' },
    messages: { [SHA('003d043')]: 'fix: adopt org_owner scopes\n\nRefs #245. Refs #245.' },
    linked: { refs: [L(245, 'OPEN')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), [`${O}/${R}#245`]);
  assert.equal(r.results[0].source, 'github-linked');
});

test('AXIS 1: the same body without the link is clean, so the link is what fails it', async () => {
  const r = await run(PR243_BODY, {
    states: { [`${O}/${R}#245`]: 'open' },
    messages: { [SHA('003d043')]: 'fix: adopt org_owner scopes\n\nRefs #245. Refs #245.' },
  });
  assert.deepEqual(ids(r), []);
});

test('AXIS 1: a Development-panel link to an ALREADY-CLOSED issue does not block the release', async () => {
  const r = await run(PR243_BODY, {
    messages: { [SHA('003d043')]: 'fix: adopt org_owner scopes\n\nRefs #245.' },
    linked: { refs: [L(245, 'CLOSED')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), []);
});

test('AXIS 1: a Development-panel link the release genuinely completes passes', async () => {
  const r = await run(PR243_BODY, {
    messages: { [SHA('003d043')]: 'fix: adopt org_owner scopes\n\nCloses #245' },
    linked: { refs: [L(245, 'OPEN')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), []);
  assert.equal(r.results[0].why, 'a commit in this release closes it deliberately');
});

test('AXIS 1: a reference in BOTH signals is graded once and labelled as both', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#393](${U(393)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#393`]: 'open' },
    linked: { refs: [L(393, 'OPEN')], hasNextPage: false },
  });
  assert.equal(r.results.length, 1);
  assert.equal(r.results[0].source, 'github-linked + body');
});

test('AXIS 1: the body scan still widens beyond GitHub, so neither signal is dropped', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#393](${U(393)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#393`]: 'open', [`${O}/${R}#245`]: 'open' },
    linked: { refs: [L(245, 'OPEN')], hasNextPage: false },
  });
  assert.deepEqual(ids(r).sort(), [`${O}/${R}#245`, `${O}/${R}#393`].sort());
});

test('AXIS 1: a cross-repository Development-panel link keeps its own owner and repo', async () => {
  const r = await run(PR243_BODY, {
    messages: { [SHA('003d043')]: 'fix: x\n\nRefs #1' },
    linked: {
      refs: [{ owner: O, repo: 'terraform-state-manager-frontend', issue: 83, state: 'OPEN' }],
      hasNextPage: false,
    },
  });
  assert.deepEqual(ids(r), [`${O}/terraform-state-manager-frontend#83`]);
});

// -- AXIS 1 floors: the authoritative read must not fail open ----------------

test('AXIS 1 floor: no closingIssuesReferences reader at all is refused', async () => {
  // Asserts the WIRING diagnostic specifically, not merely that something threw.
  // Calling an undefined reader also throws, and the catch below would dress that
  // up as a Blind -- so a test that only checked `rejects(..., Blind)` would pass
  // with this floor deleted, and could not tell a missing reader from a 502.
  await assert.rejects(
    () =>
      evaluate({
        body: PR243_BODY,
        owner: O,
        repo: R,
        issueState: async () => 'closed',
        commitMessage: async () => 'chore: x',
      }),
    (err) =>
      err instanceof Blind &&
      /No closingIssuesReferences reader was supplied/.test(err.message)
  );
});

test('AXIS 1 floor: a failing GraphQL call is refused, not silently downgraded to the body', async () => {
  await assert.rejects(
    () =>
      run(PR243_BODY, {
        linked: async () => {
          throw new Error('HTTP 502');
        },
      }),
    Blind
  );
});

test('AXIS 1 floor: a malformed GraphQL answer is refused', async () => {
  await assert.rejects(() => run(PR243_BODY, { linked: { hasNextPage: false } }), Blind);
});

test('AXIS 1 floor: a truncated closingIssuesReferences page is refused', async () => {
  await assert.rejects(
    () => run(PR243_BODY, { linked: { refs: [L(245, 'CLOSED')], hasNextPage: true } }),
    Blind
  );
});

test('AXIS 1 floor: an entry naming no issue number is refused', async () => {
  await assert.rejects(
    () => run(PR243_BODY, { linked: { refs: [{ owner: O, repo: R, issue: null }], hasNextPage: false } }),
    Blind
  );
});

test('AXIS 1 floor: an unknown state on a linked issue is refused', async () => {
  await assert.rejects(
    () => run(PR243_BODY, { linked: { refs: [L(245, 'MERGED')], hasNextPage: false } }),
    Blind
  );
});

test('AXIS 1: a linked issue makes the empty-commit-universe floor fire too', async () => {
  await assert.rejects(
    () => run(`${HEADER}nothing here\n`, { linked: { refs: [L(245, 'OPEN')], hasNextPage: false } }),
    Blind
  );
});

// -- parseLinked: the shape the GraphQL API really returns -------------------

test('parseLinked reads owner, repo, number and state out of the real payload', () => {
  const got = parseLinked({
    data: {
      repository: {
        pullRequest: {
          closingIssuesReferences: {
            pageInfo: { hasNextPage: false },
            nodes: [{ number: 393, state: 'OPEN', repository: { name: R, owner: { login: O } } }],
          },
        },
      },
    },
  });
  assert.deepEqual(got, { hasNextPage: false, refs: [{ owner: O, repo: R, issue: 393, state: 'OPEN' }] });
});

test('parseLinked carries hasNextPage through, so truncation reaches the floor', () => {
  const got = parseLinked({
    data: {
      repository: {
        pullRequest: {
          closingIssuesReferences: {
            pageInfo: { hasNextPage: true },
            nodes: [{ number: 1, state: 'OPEN', repository: { name: R, owner: { login: O } } }],
          },
        },
      },
    },
  });
  assert.equal(got.hasNextPage, true);
});

test('parseLinked throws on a null pullRequest rather than reporting an empty set', () => {
  assert.throws(() => parseLinked({ data: { repository: { pullRequest: null } } }));
});

test('parseLinked throws when the nodes array is missing', () => {
  assert.throws(() =>
    parseLinked({ data: { repository: { pullRequest: { closingIssuesReferences: {} } } } })
  );
});

// -- AXIS 2: the generator's real multi-reference output ---------------------
//
// Measured against closingIssuesReferences on ten merged release pull requests
// across seven repositories in this estate. In every one the first-only model
// equalled GitHub's set exactly and NO trailing reference was ever linked, so
// trailing references must not fail -- doing so would block every release. The
// downgrade is still cross-checked rather than assumed.

test('AXIS 2: the real tsm-frontend#43 shape -- closes [#34](u) [#35] -- grades 34, notes 35', async () => {
  const body = `${HEADER}* **a:** b ([aaaaaaa](${C(A)})), closes [#34](${U(34)}) [#35](${U(35)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#34`]: 'closed', [`${O}/${R}#35`]: 'open' },
    linked: { refs: [L(34, 'CLOSED')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), []);
  assert.deepEqual(r.notes.map(key), [`${O}/${R}#35`]);
  assert.deepEqual(r.graded.map((g) => g.id), [`${O}/${R}#34`]);
});

test('AXIS 2: a trailing reference GitHub DOES link is promoted and graded, not noted', async () => {
  const body = `${HEADER}* **a:** b ([aaaaaaa](${C(A)})), closes [#34](${U(34)}) [#35](${U(35)})\n`;
  const r = await run(body, {
    linked: { refs: [L(34, 'CLOSED'), L(35, 'OPEN')], hasNextPage: false },
  });
  assert.deepEqual(r.promoted, [`${O}/${R}#35`]);
  assert.deepEqual(r.notes.map(key), []);
  assert.deepEqual(ids(r), [`${O}/${R}#35`]);
});

test('AXIS 2: a promoted trailing reference the release completes still passes', async () => {
  const body = `${HEADER}* **a:** b ([aaaaaaa](${C(A)})), closes [#34](${U(34)}) [#35](${U(35)})\n`;
  const r = await run(body, {
    messages: { [A]: 'feat: a\n\nCloses #35' },
    linked: { refs: [L(34, 'CLOSED'), L(35, 'OPEN')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), []);
  assert.deepEqual(r.promoted, [`${O}/${R}#35`]);
});

test('AXIS 2: the comma-separated generator shape -- closes [#A](u), [#B](u)', () => {
  const { closing, trailing } = findClosingReferences(`, closes [#10](${U(10)}), [#11](${U(11)})`, O, R);
  assert.deepEqual(closing.map((c) => c.issue), [10]);
  assert.deepEqual(trailing.map((t) => t.issue), [11]);
});

test('AXIS 2: a run stops at the newline, so the next changelog entry is not swallowed', () => {
  const body =
    `* **a:** b ([aaaaaaa](${C(A)})), closes [#393](${U(393)})\n` +
    `* **c:** d ([bbbbbbb](${C(SHA('bbbbbbb'))})), closes [#521](${U(521)})\n`;
  const { closing, trailing } = findClosingReferences(body, O, R);
  assert.deepEqual(closing.map((c) => c.issue), [393, 521]);
  assert.deepEqual(trailing.map((t) => t.issue), []);
});

test('AXIS 2: three references behind one keyword grade only the first', async () => {
  const body = `${HEADER}* **a:** b ([aaaaaaa](${C(A)})), closes [#1](${U(1)}) [#2](${U(2)}) [#3](${U(3)})\n`;
  const r = await run(body, {
    states: { [`${O}/${R}#1`]: 'closed', [`${O}/${R}#2`]: 'open', [`${O}/${R}#3`]: 'open' },
    linked: { refs: [L(1, 'CLOSED')], hasNextPage: false },
  });
  assert.deepEqual(ids(r), []);
  assert.deepEqual(r.notes.map(key), [`${O}/${R}#2`, `${O}/${R}#3`]);
});

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
  assert.deepEqual(r.notes.map(key), [`${O}/${R}#2`]);
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
  await assert.rejects(() => run(body, { messages: { [A]: '   ' } }), Blind);
});

test('an unknown issue state is refused rather than assumed safe', async () => {
  const body = `${HEADER}* **x:** y ([aaaaaaa](${C(A)})), closes [#1](${U(1)})\n`;
  await assert.rejects(() => run(body, { states: { [`${O}/${R}#1`]: 'merged' } }), Blind);
});

test('a release with no closing references and no commits is still recognised', async () => {
  const r = await run(`${HEADER}nothing here\n`);
  assert.deepEqual(r.results, []);
});
