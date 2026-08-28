// EXECUTES the merge-backstop CLI -- the file the push trigger runs -- against
// the stub gh, end to end: resolve the pushed SHA to its pull request through
// the paginated listing, grade at the merge instant, and REPAIR. The library
// half is covered in time-of-check.test.mjs; these cases exist because the
// CLI's listing parse is its own mechanism (one compact object per line out
// of --paginate) and broke once, silently, under a different output shape.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CLI = path.join(HERE, 'merge-backstop.mjs');
const STUB = path.join(HERE, 'stub-gh.cjs');

const O = 'sethbacon';
const R = 'terraform-state-manager-backend';
const SHA = (p, f = '0') => (p + f.repeat(40)).slice(0, 40);
const COMMIT_SHA = SHA('c0ffee');
const MERGE_SHA = SHA('deadbeef', 'd');

const incident = () => ({
  pulls: [
    {
      number: 243,
      state: 'closed',
      base: 'main',
      headSha: SHA('ab243'),
      headRef: 'release-please--branches--main',
      mergeSha: MERGE_SHA,
      mergedAt: '2026-07-23T22:11:28Z',
      body:
        ':robot: I have created a release *beep* *boop*\n---\n\n' +
        `## [2.6.0](https://github.com/${O}/${R}/compare/v2.5.0...v2.6.0) (2026-07-23)\n\n### Bug Fixes\n\n` +
        `* adopt scopes ([#246](https://github.com/${O}/${R}/issues/246)) ([c0ffee0](https://github.com/${O}/${R}/commit/${COMMIT_SHA}))\n`,
      closingIssuesReferences: {
        pageInfo: { hasNextPage: false },
        nodes: [{ number: 245, state: 'CLOSED', repository: { name: R, owner: { login: O } } }],
      },
    },
  ],
  commits: { [COMMIT_SHA]: 'fix: adopt scopes\n\nRefs #245' },
  issues: { [`${O}/${R}#245`]: { state: 'closed', closed_at: '2026-07-23T22:11:29Z' } },
  statuses: {},
});

function runBackstop(data, sha) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'merge-backstop-'));
  const bin = path.join(dir, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, 'gh'), `#!/bin/sh\nexec node "${STUB}" "$@"\n`, { mode: 0o755 });
  const dataFile = path.join(dir, 'data.json');
  const logFile = path.join(dir, 'log.jsonl');
  fs.writeFileSync(dataFile, JSON.stringify(data));
  const r = spawnSync('node', [CLI, sha], {
    cwd: dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      STUB_DATA: dataFile,
      STUB_LOG: logFile,
      GH_TOKEN: 'stub',
      REPO: `${O}/${R}`,
    },
  });
  const log = fs.existsSync(logFile)
    ? fs.readFileSync(logFile, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))
    : [];
  fs.rmSync(dir, { recursive: true, force: true });
  return { status: r.status, out: `${r.stdout}\n${r.stderr}`, log };
}

test('BACKSTOP CLI: the #243 push fails, reopens #245, and says why', () => {
  const r = runBackstop(incident(), MERGE_SHA);
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /FAIL sethbacon\/terraform-state-manager-backend#245/);
  const reopen = r.log.find((e) => e.method === 'PATCH' && e.path.endsWith('/issues/245'));
  assert.ok(reopen, 'the repair is the reason the job exists, and it never happened');
  assert.equal(reopen.fields.state, 'open');
  const comment = r.log.find((e) => e.method === 'POST' && /issues\/245\/comments$/.test(e.path.split('?')[0]));
  assert.ok(comment, 'a silent reopen is half a repair');
});

test('BACKSTOP CLI: a push that is no release merge grades nothing and exits clean', () => {
  const data = incident();
  data.pulls[0].headRef = 'feature/not-a-release';
  const r = runBackstop(data, MERGE_SHA);
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /was not created by release-please; nothing to check/);
  assert.equal(r.log.filter((e) => e.method === 'PATCH').length, 0);
});

test('BACKSTOP CLI: a SHA with no pull request is named, not silently passed over', () => {
  const r = runBackstop(incident(), SHA('ffff', 'f'));
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /not associated with any pull request; nothing to grade/);
});
