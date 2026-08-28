// Fails a release-please pull request that would close an issue the release
// does not actually complete.
//
// THE DEFECT. release-please renders EVERY issue reference a commit carries as
// `, closes [#N](...)` in the changelog, including a deliberately non-closing
// `Refs #N` trailer. That changelog is the release pull request body; this
// repository squash-merges, so the body becomes the merge commit message; and
// GitHub reads a merge commit message for closing keywords. A `Refs` trailer
// therefore closes its issue anyway, one release later, attributed to a release
// nobody reads line by line.
//
// It has already fired here THREE times, and the third was found only by
// pointing this guard at the right universe:
//
//   1. Commit ca2e5b3 ends `Refs #459`; release pull request #480 rendered that
//      as `closes [#459]`; #480 merged at 2026-08-25T00:09:46Z and #459 closed
//      at 00:09:47Z, with nothing else in its timeline.
//   2. The same shape one release later on #393, the nine-root partition
//      tracker, caught by hand.
//   3. Release pull request #243 -- body 1205 bytes, NOT ONE closing keyword
//      anywhere in it -- merged at 2026-07-23T22:11:28Z, and issue #245 closed
//      at 22:11:29Z. Its only commit on the subject, 003d043, says `Refs #245`
//      TWICE. The link came from the Development panel, which writes a
//      `connected` timeline event and no body text at all.
//
// WHICH UNIVERSE THIS GUARD READS, AND WHY IT IS NOT THE BODY. Case 3 is the
// reason this file asks GitHub instead of parsing prose. `closingIssuesReferences`
// is GitHub's own answer to "what does merging this close?", so it is the
// authoritative set: it already includes Development-panel links, which no body
// scan can see, and it is computed by the parser that will actually act.
//
// The body scan is KEPT, as a clearly-labelled SECONDARY signal, because the two
// are not the same mechanism. The link graph closes issues attached to the pull
// request; the squash commit message is re-parsed on push to `main` and closes
// issues in its own right, and this repository merges with
// squash_merge_commit_message=COMMIT_MESSAGES so the body becomes that message.
// A reference GitHub declines to put in the link graph can still close through
// the commit-message path. The graded universe is therefore the UNION of the
// two, which is strictly wider than either and cannot be narrower than the old
// behaviour.
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
// changes the verb today.
//
// THE RULE. For each issue in the graded universe: it passes if the issue is
// already closed, because re-closing a closed issue is a no-op and cannot lose
// anything; it passes if some commit in this release carries a real closing
// keyword for it, because then closing it is what the author asked for; it
// fails otherwise. The rule keys on the AUTHOR'S intent recorded in the commit,
// not on the changelog's rendering of it, so `Refs` and `Closes` stop being the
// same thing the moment they reach this file.
//
// INTENT IS READ FROM TRAILERS ONLY, with `git interpret-trailers` as the
// parser. An earlier version scanned the whole commit message, and text that
// merely QUOTED a keyword -- a git-revert subject quoting "closes #245", a
// fenced code block, quoted review text -- counted as the author's ask, each
// spelling proven to flip the #243 incident from FAIL to PASS. A `Closes #N`
// in the trailer block is intent; the same words anywhere else are prose. The
// details and the pinned git behaviour live in trailer-intents.mjs.
//
// FAIL-CLOSED. Every floor below turns a guard that cannot see into a guard
// that fails, because a body it could not parse and a body with nothing wrong
// in it produce the same silence otherwise.
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import { findClosingReferences, key } from './closing-refs.mjs';
import { closingIntentsFromTrailers } from './trailer-intents.mjs';

const COMMIT_LINK_RE = /\/commit\/([0-9a-f]{7,40})(?![0-9a-f])/g;

export class Blind extends Error {}

const normaliseState = (s) => (typeof s === 'string' ? s.toLowerCase() : s);

export async function evaluate({
  body,
  owner,
  repo,
  issueState,
  commitMessage,
  linkedIssues,
}) {
  // FLOOR 0: the authoritative universe has to actually arrive. A guard that
  // silently falls back to the body scan when the GraphQL call fails is a guard
  // that reports its widest coverage while running at its narrowest -- the
  // blind-versus-clean failure this whole file exists to remove.
  if (typeof linkedIssues !== 'function') {
    throw new Blind(
      'No closingIssuesReferences reader was supplied, so the authoritative set of ' +
        'issues GitHub will close is unavailable. Refusing to grade the body alone.'
    );
  }

  let linked;
  try {
    linked = await linkedIssues();
  } catch (err) {
    throw new Blind(
      `Could not read closingIssuesReferences: ${err && err.message ? err.message : err}. ` +
        'That is the set GitHub acts on, so without it this guard cannot report a result.'
    );
  }
  if (!linked || !Array.isArray(linked.refs)) {
    throw new Blind(
      'The closingIssuesReferences reader returned no `refs` array. A malformed answer and ' +
        'an empty answer are indistinguishable downstream, so this fails instead.'
    );
  }
  // FLOOR 0b: a truncated page silently drops issues off the end of the
  // authoritative set, and the ones it drops look exactly like issues GitHub
  // will not close.
  if (linked.hasNextPage) {
    throw new Blind(
      'closingIssuesReferences reported another page. The authoritative set is truncated, ' +
        'so an issue past the page boundary would be graded as absent. Refusing to pass.'
    );
  }
  for (const ref of linked.refs) {
    if (!ref || !ref.owner || !ref.repo || !Number.isInteger(ref.issue)) {
      throw new Blind(
        `Malformed entry in closingIssuesReferences: ${JSON.stringify(ref)}. Cannot tell which ` +
          'issue it names.'
      );
    }
  }

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

  // The graded universe: GitHub's authoritative answer, widened by the body
  // scan. `source` records which signal saw each one, so a reviewer can tell a
  // Development-panel link from a rendered changelog keyword at a glance.
  const universe = new Map();
  const linkedStates = new Map();
  for (const ref of linked.refs) {
    const id = key(ref);
    universe.set(id, { ref, id, source: 'github-linked' });
    if (ref.state !== undefined) linkedStates.set(id, normaliseState(ref.state));
  }
  for (const ref of closing) {
    const id = key(ref);
    const seen = universe.get(id);
    if (seen) seen.source = 'github-linked + body';
    else universe.set(id, { ref, id, source: 'body-only' });
  }

  // The trailing references -- what sits behind an already-spent keyword -- are
  // NOT failed. GitHub closes only the first reference in a run, confirmed
  // against closingIssuesReferences on ten real release pull requests across
  // seven repositories in this estate: in every one, the parser's first-only
  // set equalled GitHub's set exactly and no trailing reference appeared.
  // Failing them would reject bodies GitHub demonstrably does not act on, and a
  // guard that invents failures is a guard people delete.
  //
  // But the downgrade is OUR model, not GitHub's, so it is cross-checked rather
  // than assumed: any trailing reference GitHub's authoritative set does contain
  // is PROMOTED into the graded universe above instead of being filed as a note.
  // Today that promotion set is empty everywhere it has been measured, which is
  // exactly why it has to be asserted -- if GitHub's parser or release-please's
  // separator ever changes, this catches it instead of quietly downgrading.
  const promoted = [];
  const notes = [];
  for (const ref of trailing) {
    const id = key(ref);
    if (universe.has(id)) {
      if (universe.get(id).source === 'github-linked') promoted.push(id);
    } else {
      notes.push(ref);
    }
  }

  const graded = [...universe.values()];

  const shas = [...new Set([...body.matchAll(COMMIT_LINK_RE)].map((m) => m[1]))];

  // FLOOR 2: a release that would close something must name the commits it
  // contains, or the intent test below has an empty universe to search and
  // every closing reference passes for the wrong reason.
  if (graded.length > 0 && shas.length === 0) {
    throw new Blind(
      `This release would close ${graded.length} issue(s) but the body names no commit links, ` +
        'so the set of commits it contains is empty and no closing intent could be confirmed ' +
        'or denied. Refusing to pass.'
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
    // Intent comes from TRAILER-POSITION lines only, parsed by git itself.
    // Running the keyword scan over the raw message minted intent from text
    // that merely QUOTES a keyword -- a git-revert subject quoting
    // 'closes #245', a fenced code block, quoted review text -- and each of
    // those flipped the #243 incident from FAIL to PASS. A revert is by
    // definition not completing the issue. See trailer-intents.mjs.
    let intentRefs;
    try {
      intentRefs = closingIntentsFromTrailers(message, owner, repo);
    } catch (err) {
      throw new Blind(
        `Could not read the trailers of ${sha}: ${err && err.message ? err.message : err}`
      );
    }
    for (const ref of intentRefs) {
      intents.add(key(ref));
    }
  }

  const results = [];
  for (const entry of graded) {
    const { ref, id, source } = entry;
    const state = linkedStates.has(id) ? linkedStates.get(id) : normaliseState(await issueState(ref));
    if (state !== 'open' && state !== 'closed') {
      throw new Blind(`Unknown state "${state}" for ${id}; cannot decide whether closing it is safe.`);
    }
    if (state === 'closed') {
      results.push({ ref, id, source, verdict: 'ok', why: 'already closed, so the merge cannot lose anything' });
    } else if (intents.has(id)) {
      results.push({ ref, id, source, verdict: 'ok', why: 'a commit in this release closes it deliberately' });
    } else {
      results.push({
        ref,
        id,
        source,
        verdict: 'fail',
        why:
          source === 'github-linked'
            ? 'open, and no commit in this release asks to close it -- GitHub will close it on merge ' +
              'through a link that appears nowhere in the body'
            : 'open, and no commit in this release asks to close it -- the changelog invented the keyword',
      });
    }
  }

  return { results, notes, promoted, shas, graded, linked: linked.refs, closing };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

const gh = (args) =>
  execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 });

const ghApi = (path) => gh(['api', path]);

const line = (s) => console.log(s);

function summarise(lines) {
  const file = process.env.GITHUB_STEP_SUMMARY;
  if (file) fs.appendFileSync(file, lines.join('\n') + '\n');
}

// `first: 100` with an explicit hasNextPage floor above. A release pull request
// linking more than 100 issues is not a thing that happens quietly, and if it
// ever does this fails rather than silently grading a prefix.
const LINKED_QUERY = `query($owner:String!,$repo:String!,$number:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$number){
      closingIssuesReferences(first:100){
        pageInfo{hasNextPage}
        nodes{number state repository{name owner{login}}}
      }
    }
  }
}`;

export function parseLinked(payload) {
  const pr = payload?.data?.repository?.pullRequest;
  if (!pr) throw new Error('GraphQL returned no pullRequest node');
  const cir = pr.closingIssuesReferences;
  if (!cir || !Array.isArray(cir.nodes)) throw new Error('GraphQL returned no closingIssuesReferences nodes');
  return {
    hasNextPage: Boolean(cir.pageInfo && cir.pageInfo.hasNextPage),
    refs: cir.nodes.map((n) => ({
      owner: n?.repository?.owner?.login,
      repo: n?.repository?.name,
      issue: n?.number,
      state: n?.state,
    })),
  };
}

async function main() {
  const [owner, repo] = (process.env.REPO || '').split('/');
  const bodyFile = process.argv[2];
  const prNumber = process.argv[3];
  if (!owner || !repo || !bodyFile || !prNumber) {
    console.error('usage: REPO=owner/repo verify.mjs <body-file> <pr-number>');
    process.exit(2);
  }
  const body = fs.readFileSync(bodyFile, 'utf8');

  const issueState = (ref) =>
    JSON.parse(ghApi(`repos/${ref.owner}/${ref.repo}/issues/${ref.issue}`)).state;
  const commitMessage = (sha) => JSON.parse(ghApi(`repos/${owner}/${repo}/commits/${sha}`)).commit.message;
  const linkedIssues = () =>
    parseLinked(
      JSON.parse(
        gh([
          'api',
          'graphql',
          '-f',
          `query=${LINKED_QUERY}`,
          '-f',
          `owner=${owner}`,
          '-f',
          `repo=${repo}`,
          '-F',
          `number=${prNumber}`,
        ])
      )
    );

  let report;
  try {
    report = await evaluate({ body, owner, repo, issueState, commitMessage, linkedIssues });
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

  line(
    `Enumerated ${report.shas.length} commit(s); GitHub's closingIssuesReferences names ` +
      `${report.linked.length} issue(s); the body scan adds ${report.graded.length - report.linked.length}. ` +
      `Grading ${report.graded.length}.`
  );
  for (const id of report.promoted) {
    line(`  promoted: ${id} sits behind a spent keyword, but GitHub links it as closing. Graded, not noted.`);
  }
  for (const t of report.notes) {
    line(`  note: ${key(t)} follows an already-spent "${t.keyword}" keyword and GitHub does not link it, so it will not close.`);
  }
  for (const r of report.results) {
    line(`  ${r.verdict === 'ok' ? 'ok  ' : 'FAIL'} ${r.id} [${r.source}]: ${r.why}`);
  }

  const failed = report.results.filter((r) => r.verdict === 'fail');
  if (failed.length === 0) {
    line('OK: every issue this release would close is one the release completes.');
    return;
  }

  summarise([
    '### This release would close issues it does not complete',
    '',
    'This repository squash-merges, so this pull request body becomes the merge commit',
    'message and GitHub reads it for closing keywords. release-please renders every',
    'issue reference a commit carries as `closes`, including a deliberately non-closing',
    '`Refs #N` trailer. Issues can also be attached through the **Development panel**,',
    'which closes them on merge while writing nothing into the body at all.',
    '',
    ...failed.map((r) =>
      r.source === 'github-linked'
        ? `- **${r.id}** is open, no commit in this release asks to close it, and it is linked ` +
          'to this pull request through the Development panel rather than the body. Detach it there.'
        : `- **${r.id}** is open, and no commit in this release asks to close it.`
    ),
    '',
    'For a body reference, change `closes` to `refs`. For a Development-panel link, remove',
    'the link. release-please regenerates the body on the next push to `main`, so this check',
    'will ask again, and it has to pass at the moment the release is merged.',
  ]);
  process.exit(1);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await main();
}
