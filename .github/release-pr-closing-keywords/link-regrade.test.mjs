// EXECUTES link-regrade.sh -- the real file the workflow runs -- against a
// stub `gh` this suite controls, and asserts OUTCOMES: exit codes, the
// summary line, and which commit statuses were posted.
//
// WHY EXECUTION AND NOT PARSING. Round one's suite had 74 cases and every one
// of them read text. Four mutations of the re-grade step were applied,
// confirmed to have changed the file and still parse, and ran INERT:
//
//   M1  per_page=100 -> per_page=1
//   M2  delete the considered-vs-open_count floor
//   M3  base=main -> base=develop
//   M4  skip release PRs entirely (release=$((release+1)); continue)
//
// M4 means the entire mechanism could be neutered invisibly. Each case below
// names the mutation it kills. The stub implements api.github.com's
// pagination semantics for real -- per_page, page, and gh's --paginate loop --
// so the 150-pull scenario is the verifier's reproduction, executed.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const SCRIPT = path.join(HERE, 'link-regrade.sh');
const STUB = path.join(HERE, 'stub-gh.cjs');

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const CONTEXT = 'release-guard/link-regrade';
const SHA = (p) => (p + '0'.repeat(40)).slice(0, 40);
const U = (n) => `https://github.com/${O}/${R}/issues/${n}`;
const C = (sha) => `https://github.com/${O}/${R}/commit/${sha}`;

const REL_SHA = SHA('e1ea5e');
const COMMIT_SHA = SHA('c0ffee');

// A release PR in the exact shape release-please emits: compare link, one
// commit link, and a rendered `closes` for an issue the commit only Refs.
function releasePull(number, { intent = 'Refs #900' } = {}) {
  return {
    number,
    state: 'open',
    base: 'main',
    headSha: REL_SHA,
    headRef: 'release-please--branches--main',
    body:
      ':robot: I have created a release *beep* *boop*\n---\n\n' +
      `## [9.9.9](https://github.com/${O}/${R}/compare/v9.9.8...v9.9.9) (2026-08-28)\n\n### Bug Fixes\n\n` +
      `* **x:** y ([#901](${U(901)})) ([c0ffee0](${C(COMMIT_SHA)})), closes [#900](${U(900)})\n`,
    closingIssuesReferences: {
      pageInfo: { hasNextPage: false },
      nodes: [{ number: 900, state: 'OPEN', repository: { name: R, owner: { login: O } } }],
    },
    _intent: intent,
  };
}

function dataset(pulls, extra = {}) {
  const rel = pulls.find((p) => p._intent);
  return {
    pulls,
    commits: { [COMMIT_SHA]: `fix(x): y\n\n${rel ? rel._intent : 'Refs #900'}` },
    issues: { [`${O}/${R}#900`]: { state: 'open', closed_at: null } },
    statuses: {},
    ...extra,
  };
}

function openPulls(n) {
  const out = [];
  for (let i = 1; i <= n; i++) {
    out.push({ number: i, state: 'open', base: 'main', headSha: SHA('ab' + i.toString(16)), headRef: `feature/x-${i}` });
  }
  return out;
}

// Runs the REAL script: stub gh first on PATH, cwd in a scratch dir so the
// body files it writes land nowhere near the checkout.
function runStep(data) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'link-regrade-'));
  const bin = path.join(dir, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, 'gh'), `#!/bin/sh\nexec node "${STUB}" "$@"\n`, { mode: 0o755 });
  const dataFile = path.join(dir, 'data.json');
  const logFile = path.join(dir, 'log.jsonl');
  fs.writeFileSync(dataFile, JSON.stringify(data));
  const work = path.join(dir, 'work');
  fs.mkdirSync(work);
  const r = spawnSync('bash', [SCRIPT], {
    cwd: work,
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      STUB_DATA: dataFile,
      STUB_LOG: logFile,
      GH_TOKEN: 'stub',
      REPO: `${O}/${R}`,
      CONTEXT,
      RUN_URL: 'http://example.invalid/run/1',
    },
  });
  const log = fs.existsSync(logFile)
    ? fs.readFileSync(logFile, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))
    : [];
  fs.rmSync(dir, { recursive: true, force: true });
  return { status: r.status, out: `${r.stdout}\n${r.stderr}`, log };
}

const postedTo = (log, sha) =>
  log.filter((e) => e.method === 'POST' && e.path.split('?')[0].endsWith(`/statuses/${sha}`)).map((e) => e.fields);

// -- THE 150-PULL REPRODUCTION (kills M3 and M4, and the pagination hole) ----
//
// The verifier's case, verbatim: 150 open pull requests, the release PR dead
// last -- past the first page of 100 -- and failing. The unpaginated step
// printed "enumerated: 100 open pull request(s), 0 release, 0 failing" and
// exited 0 without ever seeing it.
test('HARNESS: 150 open pulls -- the release PR past page one is walked, graded, and FAILED', () => {
  const pulls = openPulls(149);
  pulls.push(releasePull(150));
  const r = runStep(dataset(pulls));

  assert.equal(r.status, 0, `the tick itself stays green (the verdict is the status):\n${r.out}`);
  assert.match(r.out, /enumerated: 150 open pull request\(s\), 1 release, 1 failing\./,
    'the walk must reach all 150 and grade the one release PR');
  // M4 detector: skipping release PRs leaves no failure status on the head SHA.
  const posts = postedTo(r.log, REL_SHA);
  assert.equal(posts.length, 1, 'exactly one status lands on the release head SHA');
  assert.equal(posts[0].state, 'failure', 'the release PR closes #900 without completing it');
  assert.equal(posts[0].context, CONTEXT);
});

// M1 detector, on the same executed run: the listing the walk actually issued
// must request full pages. This reads the REQUEST STREAM the step produced,
// not the step's text -- narrow the page size and this is where it shows.
test('HARNESS: the executed listing paginates with full pages', () => {
  const pulls = openPulls(149);
  pulls.push(releasePull(150));
  const r = runStep(dataset(pulls));
  const listings = r.log.filter((e) => e.method === 'GET' && /\/pulls\?/.test(e.path));
  assert.ok(listings.length > 0, 'enumerated zero listing requests -- the harness is blind, not the step clean');
  for (const l of listings) {
    assert.ok(l.paginate, `listing was not --paginate: ${l.path}`);
    assert.match(l.path, /per_page=100/, `listing narrowed its pages: ${l.path}`);
  }
});

// M2 detector: the two derivations disagree -> the floor MUST fire. The stub
// skews the GraphQL count by one, exactly what a dropped page looks like from
// the count's side.
test('HARNESS: a walk that disagrees with the independent count refuses to pass', () => {
  const pulls = openPulls(3);
  const r = runStep(dataset(pulls, { totalCountSkew: 1 }));
  assert.equal(r.status, 1, `must exit 1 on a 3-vs-4 disagreement:\n${r.out}`);
  assert.match(r.out, /Walked 3 pull request\(s\) but the API reports 4 open/);
  assert.match(r.out, /Refusing to pass/);
});

// The count is a FLOOR, not a decoration: when GraphQL cannot answer, the
// step must refuse to run rather than compare against garbage.
test('HARNESS: an unusable GraphQL count refuses to run instead of running blind', () => {
  const r = runStep(dataset(openPulls(2), { breakTotalCount: true }));
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /no usable open-pull-request count/);
});

// A release the commits genuinely complete gets SUCCESS on its head SHA.
test('HARNESS: a release PR whose commit closes deliberately gets a success status', () => {
  const pulls = openPulls(2);
  pulls.push(releasePull(150, { intent: 'Closes #900' }));
  const r = runStep(dataset(pulls));
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /enumerated: 3 open pull request\(s\), 1 release, 0 failing\./);
  const posts = postedTo(r.log, REL_SHA);
  assert.equal(posts.length, 1);
  assert.equal(posts[0].state, 'success');
});

// The 1000-statuses-per-SHA budget: an unchanged verdict is NOT re-posted.
test('HARNESS: an unchanged verdict is read back and not re-posted', () => {
  const pulls = openPulls(1);
  const data = dataset(pulls, {
    statuses: { [pulls[0].headSha]: [{ context: CONTEXT, state: 'success' }] },
  });
  const r = runStep(data);
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /unchanged \(success\), not re-posted/);
  assert.equal(postedTo(r.log, pulls[0].headSha).length, 0, 're-posting every tick burns the 1000-status cap');
});

// Zero open pull requests is a real universe, and the independent count is
// what says so -- both derivations agree on empty, and that agreement is the
// only reason empty passes.
test('HARNESS: an empty universe passes only because the independent count agrees it is empty', () => {
  const r = runStep(dataset([]));
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /enumerated: 0 open pull request\(s\), 0 release, 0 failing\./);
});
