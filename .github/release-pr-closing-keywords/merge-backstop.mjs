// Grades a release that has ALREADY MERGED, and repairs what it closed by
// mistake.
//
// WHY THIS EXISTS. The pull-request-time guard beside this file reads the right
// universe -- GitHub's `closingIssuesReferences` -- but only at the moments a
// `pull_request` event fires: opened, edited, synchronize, reopened. An issue
// attached through the DEVELOPMENT PANEL writes a `connected` timeline event,
// and `connected` is not an activity type on ANY webhook: not on
// `pull_request`, not on `issues`. Nothing fires. So a link made after the last
// push is invisible to every event-driven check, forever.
//
// PROVEN ON THE INCIDENT THIS REPOSITORY ALREADY HAD. Release pull request #243:
//
//   22:01:36  last force-push to the head branch
//   22:01:39  last `pull_request` workflow runs -- CI, PR Checks x2
//   22:02:09  `connected`  <- the link is made, NO workflow run follows it
//   22:11:28  merge
//   22:11:29  issue #245 closes
//
// Replaying the pull-request-time guard in the state that actually held at
// 22:01:39 prints `linked=0 graded=0` and exits 0.
//
// WHAT THIS FILE ADDS. This runs on `push` to `main`, which fires AFTER the
// merge -- the one moment the answer is final and the one trigger no link
// timing, no cancelled run and no admin bypass can dodge. It cannot PREVENT the
// close. It removes the part that actually did the damage: that the close was
// SILENT. It reopens the issue, says why, and fails the run.
//
// WHICH ARTIFACT IS GRADED, AND WHY IT IS THE PULL REQUEST BODY. Not the merge
// commit message. This repository sets `squash_merge_commit_message=COMMIT_MESSAGES`,
// which means the branch's commit messages, NOT the pull request body -- and a
// release-please branch carries exactly one commit, `chore(main): release X.Y.Z`.
// Verified on both real incidents: the merge commits of #243 and #480 are
//
//     chore(main): release 2.6.0 (#243)
//     chore(main): release 3.13.0 (#480)
//
// plus a `Co-authored-by:` trailer. No changelog, no compare link, no closing
// keyword, no commit links. Both #245 and #459 were closed by the LINK GRAPH,
// not by a commit message. Grading the merge commit message would therefore
// grade an empty universe and report clean on both incidents. The pull request
// body is the artifact that carries the commit links the intent test needs, and
// it survives the merge.
//
// WHY THE STATE TEST HAD TO BE REWRITTEN FOR AFTER THE FACT. `evaluate()` passes
// any issue that is already closed, because re-closing a closed issue loses
// nothing. After the merge that reasoning inverts: the issue is closed BECAUSE
// of the merge being graded. Reading the state GitHub reports NOW would clear
// every violation this file exists to find. "Already" has to mean "before this
// merge".
import { evaluate, Blind } from './verify.mjs';

/**
 * The state an issue was in AT THE MOMENT the release merged.
 *
 * The `closed` timeline event's `commit_id` is NOT usable for this. Checked
 * against the live API for issue #245, closed by the merge of release pull
 * request #243: GitHub recorded `commit_id: null` and the merging USER as the
 * actor, because the close came through the link graph rather than through a
 * commit message. The timestamps are the only signal that survives, so this
 * compares against the merge time and never against now.
 */
export function stateAtMerge({ closedAt, mergedAt }) {
  const m = Date.parse(mergedAt);
  if (!mergedAt || Number.isNaN(m)) {
    throw new Blind(
      `Unusable merge timestamp ${JSON.stringify(mergedAt)}. Without it, "closed before this ` +
        'merge" and "closed BY this merge" are the same string, and the second one is the bug.'
    );
  }
  if (closedAt === null || closedAt === undefined) return 'open';
  const c = Date.parse(closedAt);
  if (Number.isNaN(c)) {
    throw new Blind(
      `Unusable closed_at timestamp ${JSON.stringify(closedAt)}; cannot tell whether this merge ` +
        'is what closed the issue.'
    );
  }
  // Strictly before the merge: it was already closed and the merge lost
  // nothing. At or after: it was OPEN when the merge happened, whatever it
  // reads as now. #245 closed at 22:11:29Z against a 22:11:28Z merge, so the
  // boundary case IS the incident and it must land on `open`.
  return c < m ? 'closed' : 'open';
}

/**
 * Runs the SAME `evaluate()` the pull-request-time guard runs -- same universe,
 * same intent rule, same floors -- with the clock wound back to the merge.
 *
 * The only two differences are injected, so the grading logic cannot drift
 * between the two callers.
 */
export async function gradeMergedRelease({
  owner,
  repo,
  body,
  mergedAt,
  linked,
  closedAt,
  commitMessage,
}) {
  const linkedIssues = async () => {
    const l = await linked();
    if (!l || !Array.isArray(l.refs)) {
      throw new Blind('The closingIssuesReferences reader returned no `refs` array after the merge.');
    }
    return {
      hasNextPage: Boolean(l.hasNextPage),
      // `state` is DROPPED on purpose. `evaluate()` short-circuits to the state
      // carried on the ref when there is one, and that state is GitHub's answer
      // for NOW -- after the merge, when every issue this merge closed reads
      // `CLOSED` and grades as safe. Dropping it forces the `issueState` path
      // below, which is the only one that knows what the merge did.
      refs: l.refs.map((r) => ({ owner: r.owner, repo: r.repo, issue: r.issue })),
    };
  };

  const issueState = async (ref) => stateAtMerge({ closedAt: await closedAt(ref), mergedAt });

  return evaluate({ body, owner, repo, issueState, commitMessage, linkedIssues });
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

import fs from 'node:fs';
import { execFileSync } from 'node:child_process';

const gh = (args) => execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 });
const api = (path) => JSON.parse(gh(['api', path]));
const line = (s) => console.log(s);

function summarise(lines) {
  const file = process.env.GITHUB_STEP_SUMMARY;
  if (file) fs.appendFileSync(file, lines.join('\n') + '\n');
}

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

async function main() {
  const [owner, repo] = (process.env.REPO || '').split('/');
  const sha = process.argv[2];
  if (!owner || !repo || !sha) {
    console.error('usage: REPO=owner/repo merge-backstop.mjs <merge-sha>');
    process.exit(2);
  }

  // No `|| true` anywhere below. A push that cannot be resolved to a pull
  // request must not read as a push that resolved to nothing to check.
  //
  // The associated-pulls listing is read through --paginate with a per-object
  // jq projection: gh emits one compact JSON object per line across ALL
  // pages, so a merge commit associated with more than one page of pull
  // requests still yields its release PR. Plain JSON.parse over --paginate
  // output would break on the second page -- gh concatenates arrays -- which
  // is why the lines are parsed one by one.
  const pulls = gh(['api', '--paginate', `repos/${owner}/${repo}/commits/${sha}/pulls?per_page=100`, '--jq', '.[]'])
    .split('\n')
    .filter(Boolean)
    .map((l) => JSON.parse(l));
  const pr = pulls.find((p) => p.merge_commit_sha === sha) || pulls[0];
  if (!pr) {
    line(`Commit ${sha} is not associated with any pull request; nothing to grade.`);
    return;
  }
  if (!/^release-please--branches--/.test(pr.head.ref)) {
    line(`Pull request #${pr.number} head '${pr.head.ref}' was not created by release-please; nothing to check.`);
    return;
  }
  if (!pr.merged_at) {
    line(`Pull request #${pr.number} reports no merged_at; refusing to grade a merge that did not happen.`);
    process.exit(1);
  }

  const full = api(`repos/${owner}/${repo}/pulls/${pr.number}`);
  const linked = () => {
    const payload = JSON.parse(
      gh(['api', 'graphql', '-f', `query=${LINKED_QUERY}`, '-f', `owner=${owner}`, '-f', `repo=${repo}`, '-F', `number=${pr.number}`])
    );
    const cir = payload?.data?.repository?.pullRequest?.closingIssuesReferences;
    if (!cir || !Array.isArray(cir.nodes)) throw new Error('GraphQL returned no closingIssuesReferences nodes');
    return {
      hasNextPage: Boolean(cir.pageInfo && cir.pageInfo.hasNextPage),
      refs: cir.nodes.map((n) => ({
        owner: n?.repository?.owner?.login,
        repo: n?.repository?.name,
        issue: n?.number,
      })),
    };
  };

  let report;
  try {
    report = await gradeMergedRelease({
      owner,
      repo,
      body: full.body,
      mergedAt: full.merged_at,
      linked,
      closedAt: async (ref) => api(`repos/${ref.owner}/${ref.repo}/issues/${ref.issue}`).closed_at,
      commitMessage: async (sha2) => api(`repos/${owner}/${repo}/commits/${sha2}`).commit.message,
    });
  } catch (err) {
    if (!(err instanceof Blind)) throw err;
    line(`BLIND: ${err.message}`);
    summarise(['### The post-merge backstop could not see', '', err.message]);
    process.exit(1);
  }

  line(
    `Release pull request #${pr.number} merged ${full.merged_at}. Enumerated ${report.shas.length} commit(s); ` +
      `closingIssuesReferences names ${report.linked.length}; grading ${report.graded.length} at the merge instant.`
  );
  for (const r of report.results) line(`  ${r.verdict === 'ok' ? 'ok  ' : 'FAIL'} ${r.id}: ${r.why}`);

  const failed = report.results.filter((r) => r.verdict === 'fail');
  if (failed.length === 0) {
    line('OK: this release closed only issues it completes.');
    return;
  }

  // REPAIR. The close already happened; the damage this removes is that nobody
  // saw it. Reopening is safe by construction: every entry here is an issue no
  // commit in the release asked to close.
  const reopened = [];
  for (const r of failed) {
    const path = `repos/${r.ref.owner}/${r.ref.repo}/issues/${r.ref.issue}`;
    if (api(path).state !== 'closed') continue;
    gh(['api', '-X', 'PATCH', path, '-f', 'state=open']);
    gh([
      'api', '-X', 'POST', `${path}/comments`, '-f',
      `body=Reopened automatically. Merging release pull request #${pr.number} closed this issue through ` +
        `GitHub's linked-issue graph, but no commit in that release carries a closing keyword for it -- ` +
        `only a non-closing reference. If the release really does complete this, close it by hand and say so here.\n\n` +
        `Backstop: \`.github/workflows/release-pr-guard.yml\`, job \`merge-backstop\`.`,
    ]);
    reopened.push(r.id);
  }

  summarise([
    `### Release #${pr.number} closed ${failed.length} issue(s) it does not complete`,
    '',
    ...failed.map((r) => `- **${r.id}** — ${r.why}`),
    '',
    reopened.length ? `Reopened: ${reopened.join(', ')}.` : 'Nothing needed reopening; the issues are already open.',
    '',
    'This ran AFTER the merge because a Development-panel link emits a `connected` timeline event,',
    'and `connected` is not an activity type on any webhook. No pre-merge trigger can observe it.',
  ]);
  line(reopened.length ? `Reopened ${reopened.join(', ')}.` : 'Nothing needed reopening.');
  process.exit(1);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await main();
}
