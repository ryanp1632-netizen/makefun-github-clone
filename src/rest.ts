// GitHub REST v3 under /api/v3: repositories, contents, refs, branches, commits, labels, issues,
// comments and pull requests, with GitHub's shapes, statuses and Link pagination.

import { VIEWER } from './identity.ts';
import { ZERO, parseIdent } from './gitobj.ts';
import * as S from './store.ts';
import type { Issue, Pull, Repo } from './store.ts';

export class HttpError extends Error {
  status: number;
  extra: Record<string, unknown>;
  constructor(status: number, message: string, extra: Record<string, unknown> = {}) {
    super(message);
    this.status = status;
    this.extra = extra;
  }
}

export interface Ctx {
  method: string;
  path: string;
  query: URLSearchParams;
  body: unknown;
  origin: string;
}

export interface Reply {
  status: number;
  body?: unknown;
  /** Pre-serialized JSON, used instead of `body` when set (the hot read path). */
  raw?: string;
  headers?: Record<string, string>;
}

// ------------------------------------------------------------------ views

export function userView(origin: string): Record<string, unknown> {
  return {
    login: VIEWER.login,
    id: VIEWER.id,
    node_id: S.globalId('U', VIEWER.id),
    type: 'User',
    site_admin: false,
    name: VIEWER.name,
    email: VIEWER.email,
    url: `${origin}/api/v3/users/${VIEWER.login}`,
    html_url: `${origin}/${VIEWER.login}`,
    avatar_url: '',
  };
}

let ownerCache: { origin: string; value: Record<string, unknown> } | null = null;
function owner(origin: string): Record<string, unknown> {
  if (ownerCache === null || ownerCache.origin !== origin) {
    ownerCache = {
      origin,
      value: { login: VIEWER.login, id: VIEWER.id, node_id: S.globalId('U', VIEWER.id), type: 'User', site_admin: false, url: `${origin}/api/v3/users/${VIEWER.login}`, html_url: `${origin}/${VIEWER.login}`, avatar_url: '' },
    };
  }
  return ownerCache.value;
}

export function repoView(r: Repo, origin: string): Record<string, unknown> {
  const api = `${origin}/api/v3/repos/${r.owner}/${r.name}`;
  let open = 0;
  for (const i of r.issues) if (i.state === 'open') open += 1;
  return {
    id: r.id,
    node_id: S.globalId('R', r.id),
    name: r.name,
    full_name: `${r.owner}/${r.name}`,
    owner: owner(origin),
    private: r.private,
    visibility: r.private ? 'private' : 'public',
    description: r.description,
    fork: false,
    archived: false,
    disabled: false,
    default_branch: r.defaultBranch,
    url: api,
    html_url: `${origin}/${r.owner}/${r.name}`,
    clone_url: `${origin}/${r.owner}/${r.name}.git`,
    git_url: `${origin}/${r.owner}/${r.name}.git`,
    issues_url: `${api}/issues{/number}`,
    pulls_url: `${api}/pulls{/number}`,
    created_at: r.created,
    updated_at: r.updated,
    pushed_at: r.pushed,
    size: r.objects.size,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    open_issues_count: open,
    open_issues: open,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

function labelView(l: S.Label, r: Repo, origin: string): Record<string, unknown> {
  return {
    id: l.id,
    node_id: S.globalId('LA', l.id),
    url: `${origin}/api/v3/repos/${r.owner}/${r.name}/labels/${encodeURIComponent(l.name)}`,
    name: l.name,
    color: l.color,
    description: l.description,
    default: false,
  };
}

export function issueView(i: Issue, origin: string): Record<string, unknown> {
  const r = i.repo;
  const api = `${origin}/api/v3/repos/${r.owner}/${r.name}`;
  const v: Record<string, unknown> = {
    id: i.id,
    node_id: S.globalId('I', i.id),
    url: `${api}/issues/${i.number}`,
    html_url: `${origin}/${r.owner}/${r.name}/${i.pull ? 'pull' : 'issues'}/${i.number}`,
    number: i.number,
    title: i.title,
    body: i.body,
    state: i.state,
    state_reason: i.state === 'closed' ? 'completed' : null,
    locked: false,
    user: owner(origin),
    labels: i.labels.map((l) => labelView(l, r, origin)),
    assignee: null,
    assignees: [],
    milestone: null,
    comments: i.comments.length,
    created_at: i.created,
    updated_at: i.updated,
    closed_at: i.closed,
    author_association: 'OWNER',
  };
  if (i.pull) v.pull_request = { url: `${api}/pulls/${i.number}`, merged_at: i.pull.mergedAt };
  return v;
}

function issueJson(i: Issue, origin: string): string {
  if (i.json === undefined) i.json = JSON.stringify(issueView(i, origin));
  return i.json;
}

function commentView(c: S.Comment, origin: string): Record<string, unknown> {
  const r = c.issue.repo;
  return {
    id: c.id,
    node_id: S.globalId('IC', c.id),
    url: `${origin}/api/v3/repos/${r.owner}/${r.name}/issues/comments/${c.id}`,
    html_url: `${origin}/${r.owner}/${r.name}/issues/${c.issue.number}#issuecomment-${c.id}`,
    issue_url: `${origin}/api/v3/repos/${r.owner}/${r.name}/issues/${c.issue.number}`,
    body: c.body,
    user: owner(origin),
    created_at: c.created,
    updated_at: c.created,
    author_association: 'OWNER',
  };
}

function branchSide(r: Repo, ref: string, sha: string, origin: string): Record<string, unknown> {
  return { label: `${r.owner}:${ref}`, ref, sha, user: owner(origin), repo: repoView(r, origin) };
}

export function pullView(p: Pull, origin: string): Record<string, unknown> {
  const i = p.issue;
  const r = i.repo;
  const api = `${origin}/api/v3/repos/${r.owner}/${r.name}`;
  const shas = S.pullShas(p);
  const mergeable = i.state === 'open' ? S.isMergeable(p) : null;
  return {
    id: p.id,
    node_id: S.globalId('PR', p.id),
    url: `${api}/pulls/${i.number}`,
    html_url: `${origin}/${r.owner}/${r.name}/pull/${i.number}`,
    diff_url: `${origin}/${r.owner}/${r.name}/pull/${i.number}.diff`,
    issue_url: `${api}/issues/${i.number}`,
    number: i.number,
    state: i.state,
    locked: false,
    title: i.title,
    body: i.body,
    user: owner(origin),
    labels: i.labels.map((l) => labelView(l, r, origin)),
    created_at: i.created,
    updated_at: i.updated,
    closed_at: i.closed,
    merged_at: p.mergedAt,
    merge_commit_sha: p.mergeSha,
    draft: false,
    head: branchSide(r, p.head, shas.head, origin),
    base: branchSide(r, p.base, shas.base, origin),
    merged: p.merged,
    mergeable,
    mergeable_state: i.state !== 'open' ? 'unknown' : mergeable ? 'clean' : 'dirty',
    merged_by: p.merged ? owner(origin) : null,
    comments: i.comments.length,
    commits: 1,
    author_association: 'OWNER',
  };
}

function commitView(r: Repo, sha: string, origin: string): Record<string, unknown> {
  const c = S.commitOf(r, sha);
  const api = `${origin}/api/v3/repos/${r.owner}/${r.name}`;
  if (c === undefined) return { sha };
  const a = parseIdent(c.author);
  const m = parseIdent(c.committer);
  return {
    sha,
    node_id: S.globalId('C', parseInt(sha.slice(0, 8), 16)),
    url: `${api}/commits/${sha}`,
    html_url: `${origin}/${r.owner}/${r.name}/commit/${sha}`,
    commit: {
      message: c.message.replace(/\n$/, ''),
      author: a,
      committer: m,
      tree: { sha: c.tree, url: `${api}/git/trees/${c.tree}` },
      url: `${api}/git/commits/${sha}`,
    },
    author: a.email === VIEWER.email ? owner(origin) : null,
    committer: m.email === VIEWER.email ? owner(origin) : null,
    parents: c.parents.map((p) => ({ sha: p, url: `${api}/commits/${p}` })),
  };
}

// ------------------------------------------------------------------ helpers

function obj(body: unknown): Record<string, unknown> {
  if (body === undefined || body === null) return {};
  if (typeof body !== 'object' || Array.isArray(body)) throw new HttpError(400, 'Problems parsing JSON');
  return body as Record<string, unknown>;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function pageParams(q: URLSearchParams): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, Number(q.get('per_page')) || 30));
  const page = Math.max(1, Number(q.get('page')) || 1);
  return { perPage, page };
}

function paginate<T>(items: T[], ctx: Ctx): { slice: T[]; headers: Record<string, string> } {
  const { perPage, page } = pageParams(ctx.query);
  const slice = items.slice((page - 1) * perPage, page * perPage);
  const last = Math.max(1, Math.ceil(items.length / perPage));
  const link = (p: number): string => {
    const q = new URLSearchParams(ctx.query);
    q.set('per_page', String(perPage));
    q.set('page', String(p));
    return `<${ctx.origin}/api/v3${ctx.path}?${q.toString()}>`;
  };
  const rels: string[] = [];
  if (page < last) rels.push(`${link(page + 1)}; rel="next"`, `${link(last)}; rel="last"`);
  if (page > 1) rels.push(`${link(1)}; rel="first"`, `${link(page - 1)}; rel="prev"`);
  return { slice, headers: rels.length > 0 ? { link: rels.join(', ') } : {} };
}

function ownRepo(ownerName: string, name: string): Repo {
  const r = S.getRepo(ownerName, name);
  if (r === undefined || r.owner.toLowerCase() !== ownerName.toLowerCase()) throw new HttpError(404, 'Not Found');
  return r;
}

function issueOf(r: Repo, num: string): Issue {
  const n = Number(num);
  const i = Number.isInteger(n) ? r.byNumber.get(n) : undefined;
  if (i === undefined) throw new HttpError(404, 'Not Found');
  return i;
}

function pullOf(r: Repo, num: string): Pull {
  const i = issueOf(r, num);
  if (i.pull === null) throw new HttpError(404, 'Not Found');
  return i.pull;
}

function decodeContent(content: unknown): Buffer {
  if (typeof content !== 'string') throw new HttpError(422, 'Invalid request.\n\n"content" wasn\'t supplied.');
  return Buffer.from(content.replace(/\s+/g, ''), 'base64');
}

function contentView(r: Repo, path: string, oid: string, origin: string, ref: string): Record<string, unknown> {
  const blob = r.objects.get(oid);
  const name = path.split('/').pop() ?? path;
  const api = `${origin}/api/v3/repos/${r.owner}/${r.name}`;
  return {
    type: 'file',
    encoding: 'base64',
    size: blob?.data.length ?? 0,
    name,
    path,
    content: blob?.data.toString('base64') ?? '',
    sha: oid,
    url: `${api}/contents/${path}?ref=${ref}`,
    git_url: `${api}/git/blobs/${oid}`,
    html_url: `${origin}/${r.owner}/${r.name}/blob/${ref}/${path}`,
    download_url: null,
  };
}

function entryView(r: Repo, dir: string, e: { mode: string; name: string; oid: string }, origin: string): Record<string, unknown> {
  const path = dir === '' ? e.name : `${dir}/${e.name}`;
  const isDir = e.mode === '40000';
  return {
    type: isDir ? 'dir' : e.mode === '160000' ? 'submodule' : 'file',
    size: isDir ? 0 : r.objects.get(e.oid)?.data.length ?? 0,
    name: e.name,
    path,
    sha: e.oid,
    url: `${origin}/api/v3/repos/${r.owner}/${r.name}/contents/${path}`,
  };
}

function branchName(r: Repo, b: unknown): string {
  return typeof b === 'string' && b !== '' ? b.replace(/^refs\/heads\//, '') : r.defaultBranch;
}

// ------------------------------------------------------------------ issue listing (shared with MCP)

export function listIssueItems(r: Repo, opts: { state?: string; sort?: string; direction?: string; labels?: string; pulls?: boolean }): Issue[] {
  const state = opts.state ?? 'open';
  const sort = opts.sort ?? 'created';
  const dir = opts.direction ?? 'desc';
  const want = (opts.labels ?? '').split(',').map((s) => s.trim().toLowerCase()).filter((s) => s !== '');
  const items = r.issues.filter((i) => (state === 'all' || i.state === state) && (opts.pulls !== false || i.pull === null) && want.every((w) => i.labels.some((l) => l.name.toLowerCase() === w)));
  const keyOf = (i: Issue): number => (sort === 'updated' ? Date.parse(i.updated) : sort === 'comments' ? i.comments.length : i.seq);
  items.sort((a, b) => (keyOf(a) - keyOf(b) || a.seq - b.seq) * (dir === 'asc' ? 1 : -1));
  return items;
}

export function createIssueChecked(r: Repo, title: unknown, body: unknown, labelNames: unknown): Issue {
  if (typeof title !== 'string' || title.trim() === '') throw new HttpError(422, 'Validation Failed', { errors: [{ resource: 'Issue', field: 'title', code: 'missing_field' }] });
  const labels: S.Label[] = [];
  if (labelNames !== undefined && labelNames !== null) {
    if (!Array.isArray(labelNames)) throw new HttpError(422, 'Validation Failed: labels must be an array');
    for (const raw of labelNames) {
      const name = typeof raw === 'string' ? raw : typeof raw === 'object' && raw !== null ? str((raw as Record<string, unknown>).name) : undefined;
      const label = name === undefined ? undefined : r.labels.get(name.toLowerCase());
      if (label === undefined) throw new HttpError(422, `Validation Failed: label ${JSON.stringify(name ?? raw)} does not exist`, { errors: [{ resource: 'Label', field: 'name', code: 'invalid', value: name ?? raw }] });
      if (!labels.includes(label)) labels.push(label);
    }
  }
  return S.createIssue(r, title, typeof body === 'string' ? body : '', labels);
}

export function createPullChecked(r: Repo, title: unknown, body: unknown, headRaw: unknown, baseRaw: unknown): Pull {
  if (typeof headRaw !== 'string' || headRaw === '') throw new HttpError(422, 'Validation Failed: head is required');
  const head = headRaw.includes(':') ? headRaw.slice(headRaw.indexOf(':') + 1) : headRaw;
  const base = typeof baseRaw === 'string' && baseRaw !== '' ? baseRaw : r.defaultBranch;
  if (!r.refs.has(`refs/heads/${head}`)) throw new HttpError(422, `Validation Failed: head branch ${JSON.stringify(head)} does not exist`, { errors: [{ resource: 'PullRequest', field: 'head', code: 'invalid' }] });
  if (!r.refs.has(`refs/heads/${base}`)) throw new HttpError(422, `Validation Failed: base branch ${JSON.stringify(base)} does not exist`, { errors: [{ resource: 'PullRequest', field: 'base', code: 'invalid' }] });
  for (const i of r.issues) {
    if (i.pull !== null && i.state === 'open' && i.pull.head === head && i.pull.base === base) {
      throw new HttpError(422, `Validation Failed: A pull request already exists for ${r.owner}:${head}.`, { errors: [{ resource: 'PullRequest', code: 'custom', message: `A pull request already exists for ${r.owner}:${head}.` }] });
    }
  }
  return S.createPull(r, typeof title === 'string' && title !== '' ? title : head, typeof body === 'string' ? body : '', head, base);
}

export function createRepoChecked(body: Record<string, unknown>): Repo {
  const name = body.name;
  if (!S.validRepoName(name)) throw new HttpError(422, 'Repository creation failed: name is invalid', { errors: [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name is invalid' }] });
  if (S.getRepo(VIEWER.login, name) !== undefined) {
    throw new HttpError(422, 'Repository creation failed: name already exists on this account', { errors: [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }] });
  }
  return S.createRepo(name, { description: str(body.description) ?? '', private: body.private === true || body.visibility === 'private', autoInit: body.auto_init === true });
}

// ------------------------------------------------------------------ the router

type Handler = (ctx: Ctx, params: string[]) => Reply;
const routes: Array<{ method: string; parts: string[]; handler: Handler }> = [];

function route(method: string, pattern: string, handler: Handler): void {
  routes.push({ method, parts: pattern.split('/').filter((p) => p !== ''), handler });
}

function ok(body: unknown, status = 200, headers?: Record<string, string>): Reply {
  return { status, body, headers };
}

route('GET', '/', (ctx) => ok({
  current_user_url: `${ctx.origin}/api/v3/user`,
  repository_url: `${ctx.origin}/api/v3/repos/{owner}/{repo}`,
  user_repositories_url: `${ctx.origin}/api/v3/user/repos`,
  issues_url: `${ctx.origin}/api/v3/issues`,
  rate_limit_url: `${ctx.origin}/api/v3/rate_limit`,
}));
route('GET', '/meta', () => ok({ verifiable_password_authentication: false, installed_version: '3.14.0-forge.1' }));
route('GET', '/rate_limit', () => ok({ resources: { core: { limit: 5000, remaining: 5000, reset: 0, used: 0 } }, rate: { limit: 5000, remaining: 5000, reset: 0, used: 0 } }));
route('GET', '/user', (ctx) => ok(userView(ctx.origin)));
route('GET', '/users/:u', (ctx, [u]) => {
  if (u.toLowerCase() !== VIEWER.login.toLowerCase()) throw new HttpError(404, 'Not Found');
  return ok(userView(ctx.origin));
});

route('GET', '/user/repos', (ctx) => {
  const all = S.listRepos(VIEWER.login).sort((a, b) => (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : a.name.toLowerCase() > b.name.toLowerCase() ? 1 : 0));
  const { slice, headers } = paginate(all, ctx);
  return ok(slice.map((r) => repoView(r, ctx.origin)), 200, headers);
});
route('GET', '/users/:u/repos', (ctx, [u]) => {
  const all = S.listRepos(u).sort((a, b) => (a.name < b.name ? -1 : 1));
  const { slice, headers } = paginate(all, ctx);
  return ok(slice.map((r) => repoView(r, ctx.origin)), 200, headers);
});
route('POST', '/user/repos', (ctx) => ok(repoView(createRepoChecked(obj(ctx.body)), ctx.origin), 201));

route('GET', '/repos/:o/:r', (ctx, [o, n]) => ok(repoView(ownRepo(o, n), ctx.origin)));
route('PATCH', '/repos/:o/:r', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  if (typeof b.description === 'string' || b.description === null) r.description = (b.description as string | null) ?? '';
  if (typeof b.private === 'boolean') r.private = b.private;
  if (typeof b.default_branch === 'string' && b.default_branch !== '') r.defaultBranch = b.default_branch;
  S.touch(r);
  return ok(repoView(r, ctx.origin));
});
route('DELETE', '/repos/:o/:r', (_ctx, [o, n]) => {
  S.deleteRepo(ownRepo(o, n));
  return { status: 204 };
});

// --- contents
function contentsGet(ctx: Ctx, o: string, n: string, path: string): Reply {
  const r = ownRepo(o, n);
  const ref = ctx.query.get('ref') ?? r.defaultBranch;
  const commit = S.resolve(r, ref);
  if (commit === undefined) throw new HttpError(404, r.refs.size === 0 ? 'This repository is empty.' : 'No commit found for the ref');
  const found = S.lookupPath(r, commit, path);
  if (found === null) throw new HttpError(404, 'Not Found');
  if (found.kind === 'file') return ok(contentView(r, path, found.oid, ctx.origin, ref));
  const dir = path.replace(/^\/+|\/+$/g, '');
  return ok(found.entries.map((e) => entryView(r, dir, e, ctx.origin)));
}
route('GET', '/repos/:o/:r/contents', (ctx, [o, n]) => contentsGet(ctx, o, n, ''));
route('GET', '/repos/:o/:r/contents/*', (ctx, [o, n, p]) => contentsGet(ctx, o, n, p));
route('PUT', '/repos/:o/:r/contents/*', (ctx, [o, n, path]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  const content = decodeContent(b.content);
  const branch = branchName(r, b.branch);
  const ref = `refs/heads/${branch}`;
  if (!r.refs.has(ref) && r.refs.size > 0) {
    const from = r.refs.get(`refs/heads/${r.defaultBranch}`);
    if (from === undefined) throw new HttpError(404, `Branch ${branch} not found`);
    S.setRef(r, ref, from);
  }
  const tip = r.refs.get(ref);
  const existing = tip === undefined ? null : S.lookupPath(r, tip, path);
  if (existing !== null && existing.kind === 'dir') throw new HttpError(422, 'path is a directory');
  const sha = str(b.sha);
  if (existing !== null) {
    if (sha === undefined) throw new HttpError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
    if (sha !== existing.oid) throw new HttpError(409, `${path} does not match ${sha}`);
  } else if (sha !== undefined && tip !== undefined) {
    throw new HttpError(409, `${path} does not exist at ${sha}`);
  }
  const message = str(b.message) ?? `Update ${path}`;
  const { commit, blobs } = S.commitFiles(r, branch, [{ path, content }], message);
  const blob = blobs.get(path) as string;
  return ok({ content: contentView(r, path, blob, ctx.origin, branch), commit: commitView(r, commit, ctx.origin) }, existing === null ? 201 : 200);
});
route('DELETE', '/repos/:o/:r/contents/*', (ctx, [o, n, path]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  const branch = branchName(r, b.branch);
  const tip = r.refs.get(`refs/heads/${branch}`);
  const existing = tip === undefined ? null : S.lookupPath(r, tip, path);
  if (existing === null || existing.kind !== 'file') throw new HttpError(404, 'Not Found');
  const sha = str(b.sha);
  if (sha === undefined) throw new HttpError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
  if (sha !== existing.oid) throw new HttpError(409, `${path} does not match ${sha}`);
  const { commit } = S.commitFiles(r, branch, [{ path, content: null }], str(b.message) ?? `Delete ${path}`);
  return ok({ content: null, commit: commitView(r, commit, ctx.origin) });
});
route('GET', '/repos/:o/:r/readme', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const commit = S.resolve(r, ctx.query.get('ref') ?? undefined);
  if (commit === undefined) throw new HttpError(404, 'Not Found');
  const root = S.lookupPath(r, commit, '');
  const e = root !== null && root.kind === 'dir' ? root.entries.find((x) => /^readme(\.|$)/i.test(x.name)) : undefined;
  if (e === undefined) throw new HttpError(404, 'Not Found');
  return ok(contentView(r, e.name, e.oid, ctx.origin, ctx.query.get('ref') ?? r.defaultBranch));
});

// --- refs, branches, commits
function refView(r: Repo, ref: string, sha: string, origin: string): Record<string, unknown> {
  const type = r.objects.get(sha)?.type ?? 'commit';
  return { ref, node_id: S.globalId('REF', r.id), url: `${origin}/api/v3/repos/${r.owner}/${r.name}/git/${ref}`, object: { sha, type, url: `${origin}/api/v3/repos/${r.owner}/${r.name}/git/${type}s/${sha}` } };
}
route('POST', '/repos/:o/:r/git/refs', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  const ref = str(b.ref);
  const sha = str(b.sha);
  if (ref === undefined || !/^refs\/[^\s]+$/.test(ref) || ref.split('/').length < 3) throw new HttpError(422, 'Reference name is invalid');
  if (sha === undefined || !r.objects.has(sha)) throw new HttpError(422, 'Object does not exist');
  if (r.refs.has(ref)) throw new HttpError(422, 'Reference already exists');
  S.setRef(r, ref, sha);
  return ok(refView(r, ref, sha, ctx.origin), 201);
});
function getRef(ctx: Ctx, o: string, n: string, rest: string): Reply {
  const r = ownRepo(o, n);
  const ref = `refs/${rest}`;
  const sha = r.refs.get(ref);
  if (sha !== undefined) return ok(refView(r, ref, sha, ctx.origin));
  const matches = [...r.refs.entries()].filter(([name]) => name.startsWith(`${ref}/`));
  if (matches.length === 0) throw new HttpError(404, 'Not Found');
  return ok(matches.map(([name, s]) => refView(r, name, s, ctx.origin)));
}
route('GET', '/repos/:o/:r/git/refs/*', (ctx, [o, n, rest]) => getRef(ctx, o, n, rest));
route('GET', '/repos/:o/:r/git/ref/*', (ctx, [o, n, rest]) => getRef(ctx, o, n, rest));
route('GET', '/repos/:o/:r/git/refs', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  return ok([...r.refs.entries()].sort().map(([name, s]) => refView(r, name, s, ctx.origin)));
});
route('PATCH', '/repos/:o/:r/git/refs/*', (ctx, [o, n, rest]) => {
  const r = ownRepo(o, n);
  const ref = `refs/${rest}`;
  const cur = r.refs.get(ref);
  if (cur === undefined) throw new HttpError(422, 'Reference does not exist');
  const b = obj(ctx.body);
  const sha = str(b.sha);
  if (sha === undefined || !r.objects.has(sha)) throw new HttpError(422, 'Object does not exist');
  const curC = S.peelToCommit(r, cur);
  const newC = S.peelToCommit(r, sha);
  if (b.force !== true && (curC === undefined || newC === undefined || !S.isAncestor(r, curC, newC))) throw new HttpError(422, 'Update is not a fast forward');
  S.setRef(r, ref, sha);
  return ok(refView(r, ref, sha, ctx.origin));
});
route('DELETE', '/repos/:o/:r/git/refs/*', (_ctx, [o, n, rest]) => {
  const r = ownRepo(o, n);
  const ref = `refs/${rest}`;
  if (!r.refs.has(ref)) throw new HttpError(422, 'Reference does not exist');
  S.setRef(r, ref, null);
  return { status: 204 };
});
function branchView(r: Repo, name: string, sha: string, origin: string): Record<string, unknown> {
  return { name, commit: { sha, url: `${origin}/api/v3/repos/${r.owner}/${r.name}/commits/${sha}` }, protected: false };
}
route('GET', '/repos/:o/:r/branches', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const list = [...r.refs.entries()].filter(([k]) => k.startsWith('refs/heads/')).sort().map(([k, s]) => branchView(r, k.slice(11), s, ctx.origin));
  const { slice, headers } = paginate(list, ctx);
  return ok(slice, 200, headers);
});
route('GET', '/repos/:o/:r/branches/*', (ctx, [o, n, b]) => {
  const r = ownRepo(o, n);
  const sha = r.refs.get(`refs/heads/${b}`);
  if (sha === undefined) throw new HttpError(404, 'Branch not found');
  const v = branchView(r, b, sha, ctx.origin);
  v.commit = commitView(r, sha, ctx.origin);
  return ok(v);
});
route('GET', '/repos/:o/:r/commits', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  if (r.refs.size === 0) throw new HttpError(409, 'Git Repository is empty.');
  const tip = S.resolve(r, ctx.query.get('sha') ?? undefined);
  if (tip === undefined) throw new HttpError(404, 'No commit found for SHA');
  const { perPage, page } = pageParams(ctx.query);
  const all = S.history(r, tip, perPage * page + 1);
  const { slice, headers } = paginate(all, ctx);
  let filtered = slice;
  const path = ctx.query.get('path');
  if (path !== null && path !== '') {
    filtered = slice.filter((sha) => {
      const c = S.commitOf(r, sha);
      const now = S.lookupPath(r, sha, path);
      const before = c && c.parents[0] ? S.lookupPath(r, c.parents[0], path) : null;
      return (now?.oid ?? null) !== (before?.oid ?? null);
    });
  }
  return ok(filtered.map((sha) => commitView(r, sha, ctx.origin)), 200, headers);
});
route('GET', '/repos/:o/:r/commits/:sha', (ctx, [o, n, sha]) => {
  const r = ownRepo(o, n);
  const c = S.resolve(r, sha);
  if (c === undefined) throw new HttpError(422, `No commit found for SHA: ${sha}`);
  return ok(commitView(r, c, ctx.origin));
});
route('GET', '/repos/:o/:r/git/commits/:sha', (ctx, [o, n, sha]) => {
  const r = ownRepo(o, n);
  if (S.commitOf(r, sha) === undefined) throw new HttpError(404, 'Not Found');
  const v = commitView(r, sha, ctx.origin);
  return ok({ sha, ...(v.commit as Record<string, unknown>), parents: v.parents });
});
route('GET', '/repos/:o/:r/git/trees/:sha', (ctx, [o, n, sha]) => {
  const r = ownRepo(o, n);
  const commit = S.resolve(r, sha);
  const treeOid = commit !== undefined ? S.commitOf(r, commit)?.tree : sha;
  if (treeOid === undefined || r.objects.get(treeOid)?.type !== 'tree') throw new HttpError(404, 'Not Found');
  const recursive = ctx.query.get('recursive');
  const tree = recursive !== null && recursive !== '0' && recursive !== 'false'
    ? [...S.flattenTree(r, treeOid)].map(([path, e]) => ({ path, mode: e.mode, type: 'blob', sha: e.oid, size: r.objects.get(e.oid)?.data.length ?? 0 }))
    : (S.lookupPath(r, commit ?? '', '') as { entries: Array<{ mode: string; name: string; oid: string }> } | null)?.entries.map((e) => ({ path: e.name, mode: e.mode, type: e.mode === '40000' ? 'tree' : 'blob', sha: e.oid })) ?? [];
  return ok({ sha: treeOid, tree, truncated: false });
});
route('GET', '/repos/:o/:r/git/blobs/:sha', (ctx, [o, n, sha]) => {
  const r = ownRepo(o, n);
  const b = r.objects.get(sha);
  if (b === undefined || b.type !== 'blob') throw new HttpError(404, 'Not Found');
  return ok({ sha, size: b.data.length, encoding: 'base64', content: b.data.toString('base64'), url: `${ctx.origin}/api/v3/repos/${r.owner}/${r.name}/git/blobs/${sha}` });
});

// --- labels
route('GET', '/repos/:o/:r/labels', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  return ok([...r.labels.values()].map((l) => labelView(l, r, ctx.origin)));
});
route('POST', '/repos/:o/:r/labels', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  const name = str(b.name);
  if (name === undefined || name.trim() === '') throw new HttpError(422, 'Validation Failed: name is required', { errors: [{ resource: 'Label', field: 'name', code: 'missing_field' }] });
  if (r.labels.has(name.toLowerCase())) throw new HttpError(422, 'Validation Failed: label already exists', { errors: [{ resource: 'Label', field: 'name', code: 'already_exists' }] });
  const color = (str(b.color) ?? 'ededed').replace(/^#/, '');
  return ok(labelView(S.addLabel(r, name, color, str(b.description) ?? ''), r, ctx.origin), 201);
});
route('GET', '/repos/:o/:r/labels/:name', (ctx, [o, n, name]) => {
  const r = ownRepo(o, n);
  const l = r.labels.get(name.toLowerCase());
  if (l === undefined) throw new HttpError(404, 'Not Found');
  return ok(labelView(l, r, ctx.origin));
});

// --- issues
route('GET', '/repos/:o/:r/issues', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const q = ctx.query;
  const items = listIssueItems(r, { state: q.get('state') ?? undefined, sort: q.get('sort') ?? undefined, direction: q.get('direction') ?? undefined, labels: q.get('labels') ?? undefined });
  const { slice, headers } = paginate(items, ctx);
  return { status: 200, raw: `[${slice.map((i) => issueJson(i, ctx.origin)).join(',')}]`, headers };
});
route('POST', '/repos/:o/:r/issues', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  const issue = createIssueChecked(r, b.title, b.body, b.labels);
  return { status: 201, raw: issueJson(issue, ctx.origin) };
});
route('GET', '/repos/:o/:r/issues/:n', (ctx, [o, n, num]) => ({ status: 200, raw: issueJson(issueOf(ownRepo(o, n), num), ctx.origin) }));
route('PATCH', '/repos/:o/:r/issues/:n', (ctx, [o, n, num]) => {
  const r = ownRepo(o, n);
  const i = issueOf(r, num);
  const b = obj(ctx.body);
  if (typeof b.title === 'string') i.title = b.title;
  if (typeof b.body === 'string' || b.body === null) i.body = (b.body as string | null) ?? '';
  if (b.labels !== undefined) i.labels = createLabelsList(r, b.labels);
  if (b.state === 'open' || b.state === 'closed') S.setIssueState(i, b.state);
  else if (b.state !== undefined) throw new HttpError(422, 'Validation Failed: state must be open or closed');
  i.updated = S.nextTime().iso;
  i.json = undefined;
  return ok(issueView(i, ctx.origin));
});
function createLabelsList(r: Repo, names: unknown): S.Label[] {
  if (!Array.isArray(names)) throw new HttpError(422, 'Validation Failed: labels must be an array');
  const out: S.Label[] = [];
  for (const raw of names) {
    const name = typeof raw === 'string' ? raw : str((raw as Record<string, unknown>)?.name);
    const l = name === undefined ? undefined : r.labels.get(name.toLowerCase());
    if (l === undefined) throw new HttpError(422, `Validation Failed: label ${JSON.stringify(name)} does not exist`);
    out.push(l);
  }
  return out;
}
route('POST', '/repos/:o/:r/issues/:n/labels', (ctx, [o, n, num]) => {
  const r = ownRepo(o, n);
  const i = issueOf(r, num);
  const b = ctx.body;
  const list = Array.isArray(b) ? b : obj(b).labels;
  for (const l of createLabelsList(r, list)) if (!i.labels.includes(l)) i.labels.push(l);
  i.json = undefined;
  return ok(i.labels.map((l) => labelView(l, r, ctx.origin)));
});
route('GET', '/repos/:o/:r/issues/:n/comments', (ctx, [o, n, num]) => {
  const i = issueOf(ownRepo(o, n), num);
  const { slice, headers } = paginate(i.comments, ctx);
  return ok(slice.map((c) => commentView(c, ctx.origin)), 200, headers);
});
route('POST', '/repos/:o/:r/issues/:n/comments', (ctx, [o, n, num]) => {
  const i = issueOf(ownRepo(o, n), num);
  const b = obj(ctx.body);
  if (typeof b.body !== 'string' || b.body === '') throw new HttpError(422, 'Validation Failed: body is required');
  return ok(commentView(S.addComment(i, b.body), ctx.origin), 201);
});
route('GET', '/repos/:o/:r/issues/comments/:id', (ctx, [o, n, id]) => {
  const r = ownRepo(o, n);
  const c = S.commentsById.get(Number(id));
  if (c === undefined || c.issue.repo !== r) throw new HttpError(404, 'Not Found');
  return ok(commentView(c, ctx.origin));
});

// --- pulls
route('GET', '/repos/:o/:r/pulls', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const state = ctx.query.get('state') ?? 'open';
  const head = ctx.query.get('head');
  const base = ctx.query.get('base');
  const dir = ctx.query.get('direction') ?? 'desc';
  const pulls = r.issues
    .filter((i) => i.pull !== null && (state === 'all' || i.state === state))
    .filter((i) => head === null || i.pull?.head === head.slice(head.indexOf(':') + 1))
    .filter((i) => base === null || i.pull?.base === base)
    .sort((a, b) => (a.seq - b.seq) * (dir === 'asc' ? 1 : -1));
  const { slice, headers } = paginate(pulls, ctx);
  return ok(slice.map((i) => pullView(i.pull as Pull, ctx.origin)), 200, headers);
});
route('POST', '/repos/:o/:r/pulls', (ctx, [o, n]) => {
  const r = ownRepo(o, n);
  const b = obj(ctx.body);
  return ok(pullView(createPullChecked(r, b.title, b.body, b.head, b.base), ctx.origin), 201);
});
route('GET', '/repos/:o/:r/pulls/:n', (ctx, [o, n, num]) => ok(pullView(pullOf(ownRepo(o, n), num), ctx.origin)));
route('PATCH', '/repos/:o/:r/pulls/:n', (ctx, [o, n, num]) => {
  const p = pullOf(ownRepo(o, n), num);
  const b = obj(ctx.body);
  if (typeof b.title === 'string') p.issue.title = b.title;
  if (typeof b.body === 'string') p.issue.body = b.body;
  if (typeof b.base === 'string' && p.issue.repo.refs.has(`refs/heads/${b.base}`)) p.base = b.base;
  if ((b.state === 'open' || b.state === 'closed') && !p.merged) S.setIssueState(p.issue, b.state);
  p.issue.json = undefined;
  return ok(pullView(p, ctx.origin));
});
route('GET', '/repos/:o/:r/pulls/:n/files', (ctx, [o, n, num]) => {
  const p = pullOf(ownRepo(o, n), num);
  const r = p.issue.repo;
  const files = S.pullFiles(p).map((f) => ({
    sha: f.oid,
    filename: f.path,
    status: f.status,
    additions: f.additions,
    deletions: f.deletions,
    changes: f.additions + f.deletions,
    blob_url: `${ctx.origin}/${r.owner}/${r.name}/blob/${p.headSha}/${f.path}`,
    contents_url: `${ctx.origin}/api/v3/repos/${r.owner}/${r.name}/contents/${f.path}?ref=${p.headSha}`,
  }));
  const { slice, headers } = paginate(files, ctx);
  return ok(slice, 200, headers);
});
route('GET', '/repos/:o/:r/pulls/:n/commits', (ctx, [o, n, num]) => {
  const p = pullOf(ownRepo(o, n), num);
  const r = p.issue.repo;
  const { head, base } = S.pullShas(p);
  const out: string[] = [];
  for (const sha of S.history(r, head, 250)) {
    if (S.isAncestor(r, sha, base)) continue;
    out.push(sha);
  }
  return ok(out.reverse().map((sha) => commitView(r, sha, ctx.origin)));
});
route('GET', '/repos/:o/:r/pulls/:n/merge', (_ctx, [o, n, num]) => {
  const p = pullOf(ownRepo(o, n), num);
  if (!p.merged) throw new HttpError(404, 'Not Found');
  return { status: 204 };
});
route('PUT', '/repos/:o/:r/pulls/:n/merge', (ctx, [o, n, num]) => {
  const p = pullOf(ownRepo(o, n), num);
  const b = obj(ctx.body);
  const sha = str(b.sha);
  if (sha !== undefined && sha !== S.pullShas(p).head) throw new HttpError(409, 'Head branch was modified. Review and try the merge again.');
  const result = S.mergePull(p, str(b.merge_method) ?? 'merge', str(b.commit_title), str(b.commit_message));
  if (!result.ok) throw new HttpError(result.status, result.message);
  return ok({ sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
});

// ------------------------------------------------------------------ dispatch

// Routes indexed by method and segment count (wildcard routes kept apart), built on first use.
let index: Map<string, typeof routes> | null = null;
function candidates(method: string, count: number): typeof routes {
  if (index === null) {
    index = new Map();
    for (const rt of routes) {
      const wild = rt.parts[rt.parts.length - 1] === '*';
      const k = wild ? `${rt.method}:*` : `${rt.method}:${rt.parts.length}`;
      const list = index.get(k) ?? [];
      list.push(rt);
      index.set(k, list);
    }
  }
  const key = `${method}#${count}`;
  let combined = index.get(key);
  if (combined === undefined) {
    combined = [...(index.get(`${method}:${count}`) ?? []), ...(index.get(`${method}:*`) ?? [])];
    index.set(key, combined);
  }
  return combined;
}

export function dispatch(ctx: Ctx): Reply {
  const parts = ctx.path.split('/').filter((p) => p !== '');
  let methodMismatch = false;
  for (const rt of candidates(ctx.method, parts.length)) {
    const params: string[] = [];
    let matched = true;
    let i = 0;
    for (; i < rt.parts.length; i += 1) {
      const want = rt.parts[i];
      if (want === '*') {
        if (i >= parts.length) {
          matched = false;
          break;
        }
        params.push(parts.slice(i).map(decodeURIComponent).join('/'));
        i = parts.length;
        break;
      }
      const got = parts[i];
      if (got === undefined) {
        matched = false;
        break;
      }
      if (want.startsWith(':')) params.push(decodeURIComponent(got));
      else if (want !== got) {
        matched = false;
        break;
      }
    }
    if (!matched) continue;
    if (rt.parts[rt.parts.length - 1] !== '*' && parts.length !== rt.parts.length) continue;
    if (rt.parts[rt.parts.length - 1] === '*' && i < parts.length) continue;
    if (rt.method !== ctx.method) {
      methodMismatch = true;
      continue;
    }
    return rt.handler(ctx, params);
  }
  if (methodMismatch) throw new HttpError(404, 'Not Found');
  throw new HttpError(404, 'Not Found');
}

export { ZERO };
