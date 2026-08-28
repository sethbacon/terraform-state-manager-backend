// Fails a release-please pull request whose body would close an issue the
// release does not actually complete.
//
// THE DEFECT. release-please renders EVERY issue reference a commit carries as
// `, closes [#N](...)` in the changelog, including a deliberately non-closing
// `Refs #N` trailer. That changelog is the release pull request body; this
// repository squash-merges, so the body becomes the merge commit message; and
// GitHub reads a merge commit message for closing keywords. A `Refs` trailer
// therefore closes its issue anyway, one release later, attributed to a release
// nobody reads line by line.
//
// It has already fired here. Commit ca2e5b3 ends `Refs #459`; release pull
// request #480 rendered that as `closes [#459]`; #480 merged at
// 2026-08-25T00:09:46Z and #459 -- "Multi-organization readiness: the partition
// is built but almost nothing enforces it" -- closed at 00:09:47Z, with nothing
// else in its timeline. The same shape was caught one release later on #393,
// the nine-root partition tracker, at 2 of 9 roots.
//
// WHY THE FIX IS A GUARD AND NOT A CONFIG CHANGE. The verb is hardcoded in the
// commit partial of conventional-changelog-conventionalcommits, and
// release-please's config schema exposes changelog-sections, changelog-type,
// changelog-path and changelog-host but NOTHING that reaches commitPartial --
// it is a constructor option on DefaultChangelogNotes, and no manifest key is
// wired to it. Upstream fixed it in the library in release-please 17.10.4 by
// rewriting `, closes` to `, refs` unconditionally, but release-please-action
// v5.0.0 bundles release-please 17.6.0, and the action's own main branch is
// still on 17.6.1. There is no configuration, and no reachable action pin, that
// changes the verb today. Shipping one anyway would be a change that does
// nothing while looking like a fix.
//
// THE RULE. For each issue a body would close: it passes if the issue is
// already closed, because re-closing a closed issue is a no-op and cannot lose
// anything; it passes if some commit in this release carries a real closing
// keyword for it, because then closing it is what the author asked for; it
// fails otherwise. The rule keys on the AUTHOR'S intent recorded in the commit,
// not on the changelog's rendering of it, so `Refs` and `Closes` stop being the
// same thing the moment they reach this file.
//
// FAIL-CLOSED. Every floor below turns a guard that cannot see into a guard
// that fails, because a body it could not parse and a body with nothing wrong
// in it produce the same silence otherwise.
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import { findClosingReferences, key } from './closing-refs.mjs';

const COMMIT_LINK_RE = /\/commit\/([0-9a-f]{7,40})(?![0-9a-f])/g;

export class Blind extends Error {}

export async function evaluate({ body, owner, repo, issueState, commitMessage }) {
  // FLOOR 1: this must be a body we recognise. A release-please body always
  // carries a compare link or the generator's footer. Without one we are not
  // looking at what we think we are looking at, and every later count would be
  // an honest zero over the wrong universe.
  const recognisable =
    /\/compare\/[^\s)]+\.{3}[^\s)]+/.test(body) ||
    /release-please/i.test(body);
  if (!recognisable) {
    throw new Blind(
      'The pull request body is not a release-please changelog: it carries neither a ' +
        'compare link nor the generator footer. Refusing to report a clean result over ' +
        'a body this guard did not understand.'
    );
  }

  const { closing, trailing } = findClosingReferences(body, owner, repo);

  const shas = [...new Set([...body.matchAll(COMMIT_LINK_RE)].map((m) => m[1]))];

  // FLOOR 2: a body that closes something must name the commits that release
  // contains, or the intent test below has an empty universe to search and
  // every closing reference passes for the wrong reason.
  if (closing.length > 0 && shas.length === 0) {
    throw new Blind(
      `The body would close ${closing.length} issue(s) but names no commit links, so the ` +
        'set of commits this release contains is empty and no closing intent could be ' +
        'confirmed or denied. Refusing to pass.'
    );
  }

  const intents = new Set();
  for (const sha of shas) {
    const message = await commitMessage(sha);
    // FLOOR 3: an empty commit message is a failed lookup wearing the costume
    // of a commit that references nothing.
    if (typeof message !== 'string' || message.trim() === '') {
      throw new Blind(`Empty commit message returned for ${sha}; cannot read its trailers.`);
    }
    for (const ref of findClosingReferences(message, owner, repo).closing) {
      intents.add(key(ref));
    }
  }

  const results = [];
  for (const ref of closing) {
    const id = key(ref);
    const state = await issueState(ref);
    if (state !== 'open' && state !== 'closed') {
      throw new Blind(`Unknown state "${state}" for ${id}; cannot decide whether closing it is safe.`);
    }
    if (state === 'closed') {
      results.push({ ref, id, verdict: 'ok', why: 'already closed, so the squash cannot lose anything' });
    } else if (intents.has(id)) {
      results.push({ ref, id, verdict: 'ok', why: 'a commit in this release closes it deliberately' });
    } else {
      results.push({
        ref,
        id,
        verdict: 'fail',
        why: 'open, and no commit in this release asked to close it -- the changelog invented the keyword',
      });
    }
  }

  return { results, trailing, shas, closing };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

const gh = (path) =>
  execFileSync('gh', ['api', path], { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 });

const line = (s) => console.log(s);

function summarise(lines) {
  const file = process.env.GITHUB_STEP_SUMMARY;
  if (file) fs.appendFileSync(file, lines.join('\n') + '\n');
}

async function main() {
  const [owner, repo] = (process.env.REPO || '').split('/');
  const bodyFile = process.argv[2];
  if (!owner || !repo || !bodyFile) {
    console.error('usage: REPO=owner/repo verify.mjs <body-file>');
    process.exit(2);
  }
  const body = fs.readFileSync(bodyFile, 'utf8');

  const issueState = (ref) =>
    JSON.parse(gh(`repos/${ref.owner}/${ref.repo}/issues/${ref.issue}`)).state;
  const commitMessage = (sha) => JSON.parse(gh(`repos/${owner}/${repo}/commits/${sha}`)).commit.message;

  let report;
  try {
    report = await evaluate({ body, owner, repo, issueState, commitMessage });
  } catch (err) {
    if (!(err instanceof Blind)) throw err;
    line(`BLIND: ${err.message}`);
    summarise([
      '### The release-PR closing-keyword guard could not see',
      '',
      err.message,
      '',
      'A guard that cannot read its artifact reports exactly like a clean one, so this',
      'fails instead.',
    ]);
    process.exit(1);
  }

  line(`Enumerated ${report.shas.length} commit(s) and ${report.closing.length} closing reference(s) in the body.`);
  for (const t of report.trailing) {
    line(`  note: ${key(t)} follows an already-spent "${t.keyword}" keyword, so GitHub will not close it.`);
  }
  for (const r of report.results) {
    line(`  ${r.verdict === 'ok' ? 'ok  ' : 'FAIL'} ${r.id}: ${r.why}`);
  }

  const failed = report.results.filter((r) => r.verdict === 'fail');
  if (failed.length === 0) {
    line('OK: every issue this release PR would close is one the release completes.');
    return;
  }

  summarise([
    '### This release would close issues it does not complete',
    '',
    'This repository squash-merges, so this pull request body becomes the merge commit',
    'message and GitHub reads it for closing keywords. release-please renders every',
    'issue reference a commit carries as `closes`, including a deliberately non-closing',
    '`Refs #N` trailer.',
    '',
    ...failed.map((r) => `- **${r.id}** is open, and no commit in this release asks to close it.`),
    '',
    'Fix it on the body, not on the commits: change `closes` to `refs` for each issue',
    'listed above. release-please regenerates the body on the next push to `main`, so',
    'this check will ask again, and it has to pass at the moment the release is merged.',
  ]);
  process.exit(1);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await main();
}
