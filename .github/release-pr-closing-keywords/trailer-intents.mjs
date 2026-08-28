// Closing INTENT comes from TRAILER-POSITION lines only, and git is the parser.
//
// WHAT WENT WRONG BEFORE. evaluate() used to run the closing-keyword scan over
// the RAW commit-message text, so any text that merely QUOTES a keyword minted
// intent, and intent is the one signal that lets a release close an open
// issue. Three spellings, each proven to flip the exact #243 incident from
// FAIL to PASS:
//
//   * a git-revert subject -- Revert "fix: x (closes #245)" -- where the
//     keyword sits inside the quoted subject of the commit being UNDONE. A
//     revert is by definition not completing the issue;
//   * a fenced code block quoting a suggestion that contains "closes #245";
//   * quoted review text -- "> closes #245" -- pasted into the message body.
//
// THE RULE NOW. A commit asks to close an issue only when it says so in a
// TRAILER: the structured block at the end of the message, the place this
// estate already puts `Refs #N`. Free text never counts. `git
// interpret-trailers` is the authoritative parser -- its block detection is
// the same one `git trailer.*` tooling uses -- rather than a reimplementation
// that would drift from it. Under git's rules, a quoted subject line, a fenced
// block and a "> " quotation are all either not in the final paragraph or not
// trailer-shaped, so none of the three spellings above parses as a trailer.
// Verified behaviour this module relies on, pinned in trailer-intents.test.mjs:
//
//   'Closes #245'   as the last paragraph  -> trailer  (separator '#')
//   'Closes: #245'  as the last paragraph  -> trailer  (separator ':')
//   the three quoting spellings above      -> NO trailers at all
//   'This closes #245 at last.' mid-prose  -> NO trailers
//   a trailer followed by prose in the same paragraph -> NO trailers (git
//     rejects a mixed block unless it holds a git-generated trailer)
//
// `#` is added to trailer.separators because the conventional-commits footer
// grammar allows `token #value` as well as `token: value`, and `Closes #245`
// is exactly how closing trailers are written here. git canonicalises both to
// `token: value` on output, which loses the distinction between `Closes #245`
// and `Closes: 245`; the leading `#` is re-attached below, so the second
// spelling is accepted as intent too. That widening is deliberate and tiny: a
// bare number in the value position of a closing trailer has no other honest
// reading, and the alternative is a hand-rolled parser that diverges from git.
//
// FAIL-CLOSED. If git cannot be run, this throws; the caller converts that
// into a Blind refusal. Falling back to scanning free text would silently
// reopen all three holes above.
import { execFileSync } from 'node:child_process';
import { matchReference } from './closing-refs.mjs';

const CLOSING_TOKEN_RE = /^(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)$/i;
const SEPARATOR_RE = /^[ \t]*,?[ \t]*/;

/**
 * The issues a commit message asks to close, read from its trailers ONLY.
 *
 * Within one closing trailer's value, references are collected the same way
 * the body scan walks a run: the first reference, then only comma- or
 * space-adjacent continuations. `Closes: #245, #246` yields both; a value
 * like `245. Refs #300.` yields only #245, because the prose after the full
 * stop is not part of the ask.
 *
 * @returns {Array<{owner: string, repo: string, issue: number}>}
 */
export function closingIntentsFromTrailers(message, defaultOwner, defaultRepo) {
  let parsed;
  try {
    parsed = execFileSync(
      'git',
      ['-c', 'trailer.separators=:#', 'interpret-trailers', '--parse'],
      { input: message, encoding: 'utf8' }
    );
  } catch (err) {
    throw new Error(
      `git interpret-trailers could not run: ${err && err.message ? err.message : err}. ` +
        'Trailer position is the only accepted spelling of closing intent, and free text ' +
        'is not a fallback.'
    );
  }

  const refs = [];
  for (const rawLine of parsed.split('\n')) {
    const m = /^([^\s:]+):[ \t]*(.*)$/.exec(rawLine.trimEnd());
    if (!m || !CLOSING_TOKEN_RE.test(m[1])) continue;
    // 'Closes #245' arrives here as token "Closes", value "245": git consumed
    // the '#' as the separator. Re-attach it so one matcher reads both
    // spellings.
    const value = /^\d/.test(m[2]) ? `#${m[2]}` : m[2];
    const first = matchReference(value, 0, defaultOwner, defaultRepo);
    if (!first) continue;
    refs.push({ owner: first.owner, repo: first.repo, issue: first.issue });
    let cursor = first.end;
    for (;;) {
      const gap = SEPARATOR_RE.exec(value.slice(cursor));
      const next = matchReference(value, cursor + gap[0].length, defaultOwner, defaultRepo);
      if (!next) break;
      refs.push({ owner: next.owner, repo: next.repo, issue: next.issue });
      cursor = next.end;
    }
  }
  return refs;
}
