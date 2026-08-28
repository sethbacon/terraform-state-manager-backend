// Finds the issue references a piece of text would CLOSE if GitHub read it as a
// commit message.
//
// This is deliberately a model of GitHub's behaviour and not of the Conventional
// Commits spec, because the artifact it is pointed at -- a release-please pull
// request body -- becomes the squash-commit message verbatim, and GitHub is the
// only reader whose interpretation can close anything.
//
// WHAT COUNTS AS A KEYWORD. GitHub's linked-issue keywords are close, closes,
// closed, fix, fixes, fixed, resolve, resolves and resolved, case-insensitively,
// optionally followed by a colon. `refs`, `references`, `part of`, `see` and a
// bare `#N` are NOT keywords, which is the entire point: a `Refs #N` trailer is
// how this estate links a tracking issue a change does not finish.
//
// WHAT COUNTS AS A REFERENCE. Five spellings reach the same issue, and a matcher
// that knows only one of them reports a clean body for four of the five -- the
// blind axis this file exists to avoid:
//
//   #123
//   GH-123
//   owner/repo#123
//   https://github.com/owner/repo/issues/123
//   [#123](https://github.com/owner/repo/issues/123)   <- what release-please emits
//
// The last one is the only spelling that appears in a generated changelog, and
// it is the one a `#\d+`-only regex cannot see at all: the reference sits inside
// markdown link TEXT. release-please's own maintainers treat that spelling as
// closing -- 17.10.4 rewrites `, closes` to `, refs` in the commit partial with
// the comment "to prevent GitHub from automatically closing referenced issues
// when the release PR is merged".
//
// ONLY THE FIRST REFERENCE AFTER A KEYWORD CLOSES. `Closes #A, #B, #C` closes A
// and leaves B and C open; this estate has already lost three days re-triaging
// two issues that stayed open behind one keyword. release-please emits exactly
// that shape -- `, closes [#a](u) [#b](u)` -- so the trailing references are
// returned separately, reported, and NOT failed on. Failing them would reject
// bodies GitHub demonstrably does not act on, and a guard that invents failures
// is a guard people delete.

const KEYWORD = '(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)';

// `\s*` spans newlines on purpose. GitHub tolerates a line break between the
// keyword and the reference, and refusing to look across one would make
// `Closes\n#123` invisible here while still closing #123 there. It does not
// make a `### Bug Fixes` heading match the entry beneath it: the next
// non-whitespace character of a changelog entry is `*`, which no reference
// spelling starts with.
const KEYWORD_RE = new RegExp(`\\b${KEYWORD}\\b[ \\t]*:?\\s*`, 'gi');

const NAME = '[A-Za-z0-9_.-]+';
const REFERENCE_RE = new RegExp(
  '^' +
    '(?:\\[[ \\t]*)?' +
    '(?:' +
    `https?://github\\.com/(?<uowner>${NAME})/(?<urepo>${NAME})/issues/(?<unum>\\d+)` +
    '|' +
    `(?:(?<owner>${NAME})/(?<repo>${NAME}))?(?:#|[Gg][Hh]-)(?<num>\\d+)` +
    ')'
);

// Consumes the `](url)` tail of a markdown link so the scan can carry on to the
// next reference in the same run.
const LINK_TAIL_RE = /^\][ \t]*\([^)]*\)/;
const SEPARATOR_RE = /^[ \t]*,?[ \t]*/;

export function matchReference(text, at, defaultOwner, defaultRepo) {
  const m = REFERENCE_RE.exec(text.slice(at));
  if (!m) return null;
  const g = m.groups;
  const owner = g.unum ? g.uowner : g.owner || defaultOwner;
  const repo = g.unum ? g.urepo : g.repo || defaultRepo;
  const issue = Number(g.unum || g.num);
  let end = at + m[0].length;
  const tail = LINK_TAIL_RE.exec(text.slice(end));
  if (tail) end += tail[0].length;
  return { owner, repo, issue, raw: text.slice(at, end), end };
}

/**
 * @returns {{closing: Array, trailing: Array}} `closing` is what GitHub would
 *   act on; `trailing` is what sits behind an already-spent keyword.
 */
export function findClosingReferences(text, defaultOwner, defaultRepo) {
  const closing = [];
  const trailing = [];
  if (!text) return { closing, trailing };

  KEYWORD_RE.lastIndex = 0;
  let keyword;
  while ((keyword = KEYWORD_RE.exec(text)) !== null) {
    const first = matchReference(text, KEYWORD_RE.lastIndex, defaultOwner, defaultRepo);
    if (!first) continue;
    const verb = keyword[0].trim().replace(/:$/, '');
    closing.push({ ...first, keyword: verb, index: keyword.index });

    let cursor = first.end;
    for (;;) {
      const gap = SEPARATOR_RE.exec(text.slice(cursor));
      const next = matchReference(text, cursor + gap[0].length, defaultOwner, defaultRepo);
      if (!next) break;
      trailing.push({ ...next, keyword: verb, index: cursor });
      cursor = next.end;
    }
    // Do not re-scan the run we just consumed.
    KEYWORD_RE.lastIndex = cursor;
  }
  return { closing, trailing };
}

export const key = (ref) => `${ref.owner}/${ref.repo}#${ref.issue}`;
