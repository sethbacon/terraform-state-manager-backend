// Fails a pull request whose merge commit release-please would not be able to
// read. A message its parser rejects is dropped in silence: no changelog entry,
// no version bump, no BREAKING CHANGE footer, and no later run recovers it.
import fs from 'node:fs';
import { parser } from '@conventional-commits/parser';

const commits = fs.readFileSync(process.argv[2] ?? 'commits.ndjson', 'utf8')
  .split('\n').filter(Boolean).map(line => JSON.parse(line));

// Mirrors this repository's squash settings, COMMIT_OR_PR_TITLE and
// COMMIT_MESSAGES: a lone commit keeps its own subject and body, otherwise
// GitHub takes the PR title and lists the commit messages. Merge commits are
// left out of the body.
const authored = commits.filter(commit => commit.parents <= 1);
const source = authored.length ? authored : commits;
const lone = commits.length === 1;
const [subject, ...rest] = source[0].message.split('\n');
const header = `${lone ? subject : process.env.PR_TITLE} (#${process.env.PR_NUMBER})`;
const body = lone
  ? rest.join('\n').trim()
  : source.map(commit => `* ${commit.message}`).join('\n\n');
const message = body ? `${header}\n\n${body}` : header;

const rejects = text => {
  try {
    parser(text);
    return null;
  } catch (err) {
    return err;
  }
};

// A reference in trailer position renders as a CLOSING keyword, whatever verb
// introduces it. conventional-changelog's commit partial hardcodes the word
// `closes` for every reference it extracts (see #522), so a deliberately
// non-closing `Refs #393` becomes `closes [#393]` in the generated release body
// -- and it returns on every regeneration, because it is baked into a commit on
// `main`. Release PR #515 had to be hand-patched twice, and the failure mode of
// a careless patch is closing an issue the release did not complete.
//
// The remedy is to keep it out of the commit: a mention belongs in PROSE, which
// is not extracted as a reference. This rejects the trailer shape only -- a line
// that is nothing but `<verb> #N` with at most three leading words -- so an
// ordinary sentence that happens to name an issue is untouched, and a DELIBERATE
// close still passes.
const CLOSING_KEYWORDS = new Set([
  'close', 'closes', 'closed',
  'fix', 'fixes', 'fixed',
  'resolve', 'resolves', 'resolved',
]);
const REFERENCE_TRAILER =
  /^[ \t]*([A-Za-z][A-Za-z-]*(?:[ \t]+[A-Za-z][A-Za-z-]*){0,2})[ \t]*:?[ \t]+#(\d+)[ \t]*$/;

const referenceTrailers = text => text.split('\n').flatMap(line => {
  const m = REFERENCE_TRAILER.exec(line);
  if (!m) return [];
  const verb = m[1].split(/[ \t]+/)[0].toLowerCase();
  return CLOSING_KEYWORDS.has(verb) ? [] : [line.trim()];
});

const err = rejects(message);
if (!err) {
  const stray = referenceTrailers(message);
  if (stray.length === 0) {
    console.log(`OK: release-please can read the squash of this PR's ${commits.length} commit(s).`);
    process.exit(0);
  }
  fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, [
    '### A non-closing reference here renders as a closing keyword',
    '',
    'These lines would land in the commit this PR squashes into `main`:',
    '',
    '```', ...stray, '```',
    '',
    'conventional-changelog prints the word `closes` in front of EVERY reference',
    'it extracts, whatever verb introduced it, so `Refs #393` is rendered as',
    '`closes [#393]` in the generated release body. It is not a one-off: the',
    'reference is baked into a commit on `main`, so release-please regenerates it',
    'on every run and the release PR has to be hand-patched again each time.',
    'Patch it carelessly once and the release closes an issue it did not complete.',
    '',
    '**Move the mention into prose.** A reference inside a sentence is not',
    'extracted, so it costs nothing and survives regeneration:',
    '',
    '```text',
    'Refs #393                                   <- rendered as `closes [#393]`',
    '',
    'This builds on the partition work in #393.  <- ordinary prose, left alone',
    '```',
    '',
    'If the commit really does complete the issue, say so with a closing keyword',
    '(`Closes #393`) and this check passes -- it is not a ban on references, only',
    'on ones whose rendering contradicts what they say.',
    '',
    'Fix the commit message on the branch, not the PR description -- this',
    'repository squashes with `COMMIT_MESSAGES`, so the body comes from the',
    'commits themselves.',
  ].join('\n') + '\n');
  console.log(`::error::a non-closing reference would render as a closing keyword: ${stray.join('; ')}`);
  process.exit(1);
}

// Re-parse growing prefixes so the report can name the line that breaks it.
const lines = message.split('\n');
let culprit = null;
for (let i = 1; i < lines.length && culprit === null; i++) {
  if (rejects(lines.slice(0, i + 1).join('\n'))) culprit = lines[i];
}

const detail = String(err.message).split('\n')[0];
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, [
  '### This PR would merge as a commit release-please cannot read',
  '',
  'release-please parses commits with `@conventional-commits/parser`, a strict',
  'reading of the Conventional Commits grammar. It rejects the message this PR',
  'would squash into `main`:',
  '',
  '```', detail, '```',
  ...(culprit ? ['', 'The first line it cannot get past:', '', '```', culprit, '```'] : []),
  '',
  'A commit it cannot parse contributes nothing: no changelog entry, no version',
  'bump, and no `BREAKING CHANGE:` footer. release-please logs `commit could not',
  'be parsed`, reports success, and moves on -- and every later run re-reads the',
  'same commit and skips it again, so the change never reaches a changelog.',
  '',
  'The usual cause is a body line that **starts** with `name(`. The grammar reads',
  'that as a structured token, so its brackets have to be flat and closed: a line',
  'opening with `WithArgs(pq.Array(...))` voids the commit, while the same text one',
  'word further along the line is ordinary prose and parses. Reflow the line so it',
  'does not begin with the call, or drop the inner brackets.',
  '',
  'Fix the commit message on the branch, not the PR description -- this repository',
  'squashes with `COMMIT_MESSAGES`, so the body comes from the commits themselves.',
].join('\n') + '\n');

console.log(`::error::release-please cannot parse the commit this PR would create: ${detail}`);
process.exit(1);
