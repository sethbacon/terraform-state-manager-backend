#!/usr/bin/env node
// Stub `gh` for the link-regrade harness. NOT a test file -- node --test skips
// it -- and never invoked by the workflow; link-regrade.test.mjs puts a
// wrapper for it first on PATH and runs the REAL link-regrade.sh against it.
//
// It exists because of how round one failed: a 74-case suite PARSED the
// re-grade step's text while four mutations of the step ran inert, because
// nothing ever executed the step. This stub is the executable half of the
// fix. It serves a dataset (STUB_DATA, JSON) and records every invocation to
// STUB_LOG (JSON lines), implementing just enough of `gh api` for
// link-regrade.sh and verify.mjs: query-string pagination the way
// api.github.com does it, and --paginate the way the gh client does it
// (per-page output, concatenated; --jq applied per page).
//
// Dataset shape:
//   pulls:   [{number, state, base, headSha, headRef, body?, mergedAt?,
//              closingIssuesReferences?}]
//   commits: {sha: message}
//   issues:  {"owner/repo#n": {state, closed_at}}
//   statuses:{sha: [{context, state}]}          statuses already on a SHA
//   totalCountSkew: int                          shifts the GraphQL count, to
//                                                prove the count floor fires
//   breakTotalCount: true                        GraphQL answers null instead
//
// POSTed statuses are recorded in STUB_LOG and folded into later combined-
// status reads, so an unchanged verdict is observable as "no second POST".
'use strict';
const fs = require('fs');
const { spawnSync } = require('child_process');

const data = JSON.parse(fs.readFileSync(process.env.STUB_DATA, 'utf8'));
const logPath = process.env.STUB_LOG;

const argv = process.argv.slice(2);
if (argv[0] !== 'api') { process.stderr.write(`stub gh: unsupported subcommand ${argv[0]}\n`); process.exit(64); }

let method = 'GET', paginate = false, slurp = false, jqFilter = null, path = null;
const fields = {};
for (let i = 1; i < argv.length; i++) {
  const a = argv[i];
  if (a === '-X') method = argv[++i];
  else if (a === '--paginate') paginate = true;
  else if (a === '--slurp') slurp = true;
  else if (a === '--jq') jqFilter = argv[++i];
  else if (a === '-f' || a === '-F') { const kv = argv[++i]; const eq = kv.indexOf('='); fields[kv.slice(0, eq)] = kv.slice(eq + 1); }
  else if (!a.startsWith('-')) path = a;
  else { process.stderr.write(`stub gh: unsupported flag ${a}\n`); process.exit(64); }
}

const log = (entry) => { if (logPath) fs.appendFileSync(logPath, JSON.stringify(entry) + '\n'); };
log({ method, path, paginate, slurp, jq: jqFilter, fields });

const postedStatuses = () => {
  if (!logPath || !fs.existsSync(logPath)) return [];
  return fs.readFileSync(logPath, 'utf8').split('\n').filter(Boolean).map(JSON.parse)
    .filter((e) => e.method === 'POST' && /\/statuses\//.test(e.path));
};

function emit(obj) {
  const json = JSON.stringify(obj);
  if (!jqFilter) { process.stdout.write(json + '\n'); return; }
  // -r for raw strings, -c for compact one-line objects: what real gh emits when piped.
  const r = spawnSync('jq', ['-rc', jqFilter], { input: json, encoding: 'utf8' });
  if (r.status !== 0) { process.stderr.write(r.stderr); process.exit(1); }
  process.stdout.write(r.stdout);
}

if (path === 'graphql') {
  const q = fields.query || '';
  if (/closingIssuesReferences/.test(q)) {
    const pr = data.pulls.find((p) => p.number === Number(fields.number));
    const cir = (pr && pr.closingIssuesReferences) || { pageInfo: { hasNextPage: false }, nodes: [] };
    emit({ data: { repository: { pullRequest: { closingIssuesReferences: cir } } } });
    process.exit(0);
  }
  if (/pullRequests\(/.test(q) && /totalCount/.test(q)) {
    if (data.breakTotalCount) {
      emit({ data: { repository: { pullRequests: { totalCount: null } } } });
      process.exit(0);
    }
    const m = /baseRefName:\s*"([^"]+)"/.exec(q);
    const base = m ? m[1] : 'main';
    const n = data.pulls.filter((p) => p.state === 'open' && p.base === base).length
      + (data.totalCountSkew || 0);
    emit({ data: { repository: { pullRequests: { totalCount: n } } } });
    process.exit(0);
  }
  process.stderr.write('stub gh: unrecognised graphql query\n'); process.exit(64);
}

const [restPath, queryString] = path.split('?');
const query = Object.fromEntries((queryString || '').split('&').filter(Boolean).map((kv) => kv.split('=').map(decodeURIComponent)));
let m;

if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/pulls$/.exec(restPath))) {
  const base = query.base, state = query.state || 'open';
  const all = data.pulls.filter((p) => p.state === state && (!base || p.base === base));
  const perPage = Math.max(1, Number(query.per_page || 30));
  const project = (p) => ({ number: p.number, state: p.state, head: { sha: p.headSha, ref: p.headRef }, base: { ref: p.base } });
  const pages = [];
  for (let at = 0; at < all.length; at += perPage) pages.push(all.slice(at, at + perPage).map(project));
  if (pages.length === 0) pages.push([]);
  const wanted = paginate ? pages : [pages[Math.max(1, Number(query.page || 1)) - 1] || []];
  if (slurp) emit([].concat(...wanted));
  else for (const page of wanted) emit(page);
  process.exit(0);
}
if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)$/.exec(restPath))) {
  const pr = data.pulls.find((p) => p.number === Number(m[3]));
  if (!pr) { process.stderr.write('stub gh: no such pull\n'); process.exit(1); }
  emit({ number: pr.number, state: pr.state, body: pr.body || '', merged_at: pr.mergedAt || null,
         head: { sha: pr.headSha, ref: pr.headRef }, base: { ref: pr.base } });
  process.exit(0);
}
if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)\/status$/.exec(restPath))) {
  const sha = m[3];
  const contexts = new Map();
  for (const s of (data.statuses && data.statuses[sha]) || []) contexts.set(s.context, s);
  for (const e of postedStatuses()) if (e.path.split('?')[0].endsWith(`/statuses/${sha}`)) contexts.set(e.fields.context, { context: e.fields.context, state: e.fields.state });
  const statuses = [...contexts.values()];
  emit({ state: statuses.length ? statuses[0].state : 'pending', statuses });
  process.exit(0);
}
if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)\/pulls$/.exec(restPath))) {
  emit(data.pulls.filter((p) => p.mergeSha === m[3]).map((p) => ({ number: p.number, merge_commit_sha: p.mergeSha, merged_at: p.mergedAt || null, head: { ref: p.headRef } })));
  process.exit(0);
}
if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)$/.exec(restPath))) {
  const msg = (data.commits || {})[m[3]];
  if (msg === undefined) { process.stderr.write(`stub gh: no commit ${m[3]}\n`); process.exit(1); }
  emit({ sha: m[3], commit: { message: msg } });
  process.exit(0);
}
if (method === 'GET' && (m = /^repos\/([^/]+)\/([^/]+)\/issues\/(\d+)$/.exec(restPath))) {
  const issue = (data.issues || {})[`${m[1]}/${m[2]}#${m[3]}`];
  if (!issue) { process.stderr.write(`stub gh: no issue ${m[0]}\n`); process.exit(1); }
  emit(issue);
  process.exit(0);
}
if (method === 'POST' && /^repos\/[^/]+\/[^/]+\/statuses\/[^/]+$/.test(restPath)) { emit({ ok: true }); process.exit(0); }
if (method === 'PATCH' && /^repos\/[^/]+\/[^/]+\/issues\/\d+$/.test(restPath)) { emit({ ok: true }); process.exit(0); }
if (method === 'POST' && /issues\/\d+\/comments$/.test(restPath)) { emit({ ok: true }); process.exit(0); }

process.stderr.write(`stub gh: unhandled ${method} ${path}\n`);
process.exit(64);
