// Model Context Protocol at POST /mcp: JSON-RPC 2.0 over streamable HTTP. Speaks the 2026-07-28
// revision (server/discover, no session) and the handshake revisions (initialize +
// notifications/initialized). Tools act on the same store as REST, with github-mcp-server's names.

import { VIEWER } from './identity.ts';
import * as S from './store.ts';
import type { Repo } from './store.ts';
import {
  HttpError, createIssueChecked, createPullChecked, createRepoChecked, issueView, listIssueItems, pullView, repoView,
} from './rest.ts';

export const SUPPORTED = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const SERVER_INFO = { name: 'makefun-forge', title: 'Forge MCP', version: '1.0.0' };
const INSTRUCTIONS = 'A GitHub-compatible forge. Repositories belong to the authenticated user; use owner + repo to address one.';

interface ToolDef {
  name: string;
  description: string;
  props: Record<string, Record<string, unknown>>;
  required: string[];
  run: (a: Record<string, unknown>, origin: string) => unknown;
}

const OWNER = { type: 'string', description: 'Repository owner (user login)' };
const REPO = { type: 'string', description: 'Repository name' };

class ToolError extends Error {}

function repoArg(a: Record<string, unknown>): Repo {
  let owner = a.owner;
  let name = a.repo;
  if (typeof a.full_name === 'string' && a.full_name.includes('/')) [owner, name] = a.full_name.split('/', 2);
  if (typeof owner !== 'string' || typeof name !== 'string') throw new ToolError('owner and repo are required');
  const r = S.getRepo(owner, name);
  if (r === undefined || r.owner.toLowerCase() !== owner.toLowerCase()) throw new ToolError(`repository ${owner}/${name} not found`);
  return r;
}

function num(v: unknown, what: string): number {
  const n = typeof v === 'string' && /^\d+$/.test(v) ? Number(v) : v;
  if (typeof n !== 'number' || !Number.isInteger(n) || n < 1) throw new ToolError(`${what} must be a positive integer`);
  return n;
}

function pullOf(r: Repo, n: number): S.Pull {
  const i = r.byNumber.get(n);
  if (i === undefined || i.pull === null) throw new ToolError(`pull request #${n} not found in ${r.owner}/${r.name}`);
  return i.pull;
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    props: { name: { type: 'string', description: 'Repository name' }, description: { type: 'string', description: 'Repository description' }, private: { type: 'boolean', description: 'Whether the repository is private' }, autoInit: { type: 'boolean', description: 'Create an initial commit with a README' } },
    required: ['name'],
    run: (a, o) => repoView(createRepoChecked({ name: a.name, description: a.description, private: a.private, auto_init: a.autoInit }), o),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    props: { owner: OWNER, repo: REPO },
    required: ['owner', 'repo'],
    run: (a, o) => repoView(repoArg(a), o),
  },
  {
    name: 'list_repos',
    description: 'List the repositories of the authenticated user.',
    props: { perPage: { type: 'number', description: 'Results per page (max 100)' } },
    required: [],
    run: (_a, o) => ({ repositories: S.listRepos(VIEWER.login).map((r) => repoView(r, o)) }),
  },
  {
    name: 'create_issue',
    description: 'Create a new issue in a repository.',
    props: { owner: OWNER, repo: REPO, title: { type: 'string', description: 'Issue title' }, body: { type: 'string', description: 'Issue body (Markdown)' }, labels: { type: 'array', items: { type: 'string' }, description: 'Label names (each must exist)' } },
    required: ['owner', 'repo', 'title'],
    run: (a, o) => issueView(createIssueChecked(repoArg(a), a.title, a.body, a.labels), o),
  },
  {
    name: 'get_issue',
    description: 'Get one issue by number.',
    props: { owner: OWNER, repo: REPO, issue_number: { type: 'number', description: 'Issue number' } },
    required: ['owner', 'repo', 'issue_number'],
    run: (a, o) => {
      const r = repoArg(a);
      const i = r.byNumber.get(num(a.issue_number, 'issue_number'));
      if (i === undefined) throw new ToolError('issue not found');
      return issueView(i, o);
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, filtered by state.',
    props: { owner: OWNER, repo: REPO, state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Issue state filter (default open)' }, labels: { type: 'array', items: { type: 'string' }, description: 'Only issues carrying all these labels' }, direction: { type: 'string', enum: ['asc', 'desc'], description: 'Sort direction by creation' } },
    required: ['owner', 'repo'],
    run: (a, o) => {
      const r = repoArg(a);
      const state = typeof a.state === 'string' ? a.state.toLowerCase() : 'open';
      const items = listIssueItems(r, { state, direction: typeof a.direction === 'string' ? a.direction : 'desc', labels: Array.isArray(a.labels) ? a.labels.join(',') : undefined, pulls: false });
      return { issues: items.map((i) => issueView(i, o)), totalCount: items.length };
    },
  },
  {
    name: 'update_issue',
    description: 'Update an issue: title, body or state (open/closed).',
    props: { owner: OWNER, repo: REPO, issue_number: { type: 'number', description: 'Issue number' }, title: { type: 'string', description: 'New title' }, body: { type: 'string', description: 'New body' }, state: { type: 'string', enum: ['open', 'closed'], description: 'New state' } },
    required: ['owner', 'repo', 'issue_number'],
    run: (a, o) => {
      const r = repoArg(a);
      const i = r.byNumber.get(num(a.issue_number, 'issue_number'));
      if (i === undefined) throw new ToolError('issue not found');
      if (typeof a.title === 'string') i.title = a.title;
      if (typeof a.body === 'string') i.body = a.body;
      if (a.state === 'open' || a.state === 'closed') S.setIssueState(i, a.state);
      i.json = undefined;
      return issueView(i, o);
    },
  },
  {
    name: 'add_issue_comment',
    description: 'Add a comment to an issue or pull request.',
    props: { owner: OWNER, repo: REPO, issue_number: { type: 'number', description: 'Issue or pull request number' }, body: { type: 'string', description: 'Comment text' } },
    required: ['owner', 'repo', 'issue_number', 'body'],
    run: (a) => {
      const r = repoArg(a);
      const i = r.byNumber.get(num(a.issue_number, 'issue_number'));
      if (i === undefined) throw new ToolError('issue not found');
      if (typeof a.body !== 'string' || a.body === '') throw new ToolError('body is required');
      const c = S.addComment(i, a.body);
      return { id: c.id, body: c.body, created_at: c.created, issue_number: i.number };
    },
  },
  {
    name: 'create_branch',
    description: 'Create a branch from another branch (default: the default branch).',
    props: { owner: OWNER, repo: REPO, branch: { type: 'string', description: 'New branch name' }, from_branch: { type: 'string', description: 'Source branch' } },
    required: ['owner', 'repo', 'branch'],
    run: (a) => {
      const r = repoArg(a);
      if (typeof a.branch !== 'string' || a.branch === '') throw new ToolError('branch is required');
      const from = typeof a.from_branch === 'string' && a.from_branch !== '' ? a.from_branch : r.defaultBranch;
      const sha = r.refs.get(`refs/heads/${from}`);
      if (sha === undefined) throw new ToolError(`branch ${from} not found`);
      if (r.refs.has(`refs/heads/${a.branch}`)) throw new ToolError(`branch ${a.branch} already exists`);
      S.setRef(r, `refs/heads/${a.branch}`, sha);
      return { ref: `refs/heads/${a.branch}`, object: { sha, type: 'commit' } };
    },
  },
  {
    name: 'list_branches',
    description: 'List the branches of a repository.',
    props: { owner: OWNER, repo: REPO },
    required: ['owner', 'repo'],
    run: (a) => {
      const r = repoArg(a);
      return { branches: [...r.refs.entries()].filter(([k]) => k.startsWith('refs/heads/')).sort().map(([k, sha]) => ({ name: k.slice(11), commit: { sha } })) };
    },
  },
  {
    name: 'create_or_update_file',
    description: 'Create or update one file on a branch with a commit (pass sha to update an existing file).',
    props: { owner: OWNER, repo: REPO, path: { type: 'string', description: 'File path' }, content: { type: 'string', description: 'File content (plain text)' }, message: { type: 'string', description: 'Commit message' }, branch: { type: 'string', description: 'Branch (default: the default branch)' }, sha: { type: 'string', description: 'Current blob sha when updating' } },
    required: ['owner', 'repo', 'path', 'content', 'message'],
    run: (a) => {
      const r = repoArg(a);
      if (typeof a.path !== 'string' || a.path === '' || typeof a.content !== 'string') throw new ToolError('path and content are required');
      const branch = typeof a.branch === 'string' && a.branch !== '' ? a.branch : r.defaultBranch;
      if (!r.refs.has(`refs/heads/${branch}`) && r.refs.size > 0) {
        const from = r.refs.get(`refs/heads/${r.defaultBranch}`);
        if (from === undefined) throw new ToolError(`branch ${branch} not found`);
        S.setRef(r, `refs/heads/${branch}`, from);
      }
      const tip = r.refs.get(`refs/heads/${branch}`);
      const existing = tip === undefined ? null : S.lookupPath(r, tip, a.path);
      if (existing !== null && existing.kind === 'file' && typeof a.sha === 'string' && a.sha !== existing.oid) throw new ToolError(`sha does not match ${a.path}`);
      const { commit, blobs } = S.commitFiles(r, branch, [{ path: a.path, content: Buffer.from(a.content, 'utf8') }], typeof a.message === 'string' ? a.message : `Update ${a.path}`);
      return { content: { path: a.path, sha: blobs.get(a.path) }, commit: { sha: commit } };
    },
  },
  {
    name: 'get_file_contents',
    description: 'Get a file (decoded text) or a directory listing at a path and ref.',
    props: { owner: OWNER, repo: REPO, path: { type: 'string', description: 'Path (empty for the root)' }, ref: { type: 'string', description: 'Branch, tag or commit sha' } },
    required: ['owner', 'repo'],
    run: (a) => {
      const r = repoArg(a);
      const commit = S.resolve(r, typeof a.ref === 'string' ? a.ref : undefined);
      if (commit === undefined) throw new ToolError('ref not found or repository empty');
      const path = typeof a.path === 'string' ? a.path.replace(/^\/+/, '') : '';
      const found = S.lookupPath(r, commit, path);
      if (found === null) throw new ToolError(`${path} not found`);
      if (found.kind === 'file') return { type: 'file', path, sha: found.oid, content: r.objects.get(found.oid)?.data.toString('utf8') ?? '' };
      return { type: 'dir', path, entries: found.entries.map((e) => ({ name: e.name, type: e.mode === '40000' ? 'dir' : 'file', sha: e.oid })) };
    },
  },
  {
    name: 'list_commits',
    description: 'List commits reachable from a branch or sha, newest first.',
    props: { owner: OWNER, repo: REPO, sha: { type: 'string', description: 'Branch or commit sha (default: the default branch)' }, perPage: { type: 'number', description: 'Max commits (default 30)' } },
    required: ['owner', 'repo'],
    run: (a) => {
      const r = repoArg(a);
      const tip = S.resolve(r, typeof a.sha === 'string' ? a.sha : undefined);
      if (tip === undefined) return { commits: [] };
      const limit = typeof a.perPage === 'number' ? Math.min(100, Math.max(1, a.perPage)) : 30;
      return { commits: S.history(r, tip, limit).map((sha) => ({ sha, message: (S.commitOf(r, sha)?.message ?? '').replace(/\n$/, '') })) };
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    props: { owner: OWNER, repo: REPO, title: { type: 'string', description: 'Pull request title' }, body: { type: 'string', description: 'Pull request description' }, head: { type: 'string', description: 'Branch with the changes' }, base: { type: 'string', description: 'Branch to merge into' } },
    required: ['owner', 'repo', 'title', 'head', 'base'],
    run: (a, o) => pullView(createPullChecked(repoArg(a), a.title, a.body, a.head, a.base), o),
  },
  {
    name: 'get_pull_request',
    description: 'Get one pull request by number.',
    props: { owner: OWNER, repo: REPO, pull_number: { type: 'number', description: 'Pull request number' } },
    required: ['owner', 'repo', 'pull_number'],
    run: (a, o) => pullView(pullOf(repoArg(a), num(a.pull_number, 'pull_number')), o),
  },
  {
    name: 'list_pull_requests',
    description: 'List pull requests in a repository, filtered by state.',
    props: { owner: OWNER, repo: REPO, state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'State filter (default open)' } },
    required: ['owner', 'repo'],
    run: (a, o) => {
      const r = repoArg(a);
      const state = typeof a.state === 'string' ? a.state : 'open';
      return { pull_requests: r.issues.filter((i) => i.pull !== null && (state === 'all' || i.state === state)).map((i) => pullView(i.pull as S.Pull, o)) };
    },
  },
  {
    name: 'get_pull_request_files',
    description: 'List the files a pull request changes.',
    props: { owner: OWNER, repo: REPO, pull_number: { type: 'number', description: 'Pull request number' } },
    required: ['owner', 'repo', 'pull_number'],
    run: (a) => ({ files: S.pullFiles(pullOf(repoArg(a), num(a.pull_number, 'pull_number'))).map((f) => ({ filename: f.path, status: f.status, additions: f.additions, deletions: f.deletions, changes: f.additions + f.deletions })) }),
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request (merge_method: merge or squash).',
    props: { owner: OWNER, repo: REPO, pull_number: { type: 'number', description: 'Pull request number' }, merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method (default squash)' }, commit_title: { type: 'string', description: 'Merge commit title' }, commit_message: { type: 'string', description: 'Merge commit message' } },
    required: ['owner', 'repo', 'pull_number'],
    run: (a) => {
      const p = pullOf(repoArg(a), num(a.pull_number, 'pull_number'));
      const res = S.mergePull(p, typeof a.merge_method === 'string' ? a.merge_method : 'squash', typeof a.commit_title === 'string' ? a.commit_title : undefined, typeof a.commit_message === 'string' ? a.commit_message : undefined);
      if (!res.ok) throw new ToolError(res.message);
      return { sha: res.sha, merged: true, message: 'Pull Request successfully merged', number: p.issue.number };
    },
  },
];

const TOOL_LIST = TOOLS.map((t) => ({
  name: t.name,
  title: t.name.split('_').map((w) => w[0].toUpperCase() + w.slice(1)).join(' '),
  description: t.description,
  inputSchema: { type: 'object', properties: t.props, required: t.required, additionalProperties: false },
}));
const TOOLS_BY_NAME = new Map(TOOLS.map((t) => [t.name, t]));

type Id = string | number | null;
interface RpcOut {
  jsonrpc: '2.0';
  id: Id;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

function rpcError(id: Id, code: number, message: string, data?: unknown): RpcOut {
  return data === undefined ? { jsonrpc: '2.0', id, error: { code, message } } : { jsonrpc: '2.0', id, error: { code, message, data } };
}

function toolResult(value: unknown, isError: boolean): Record<string, unknown> {
  if (isError) return { content: [{ type: 'text', text: String(value) }], isError: true };
  const structured = typeof value === 'object' && value !== null && !Array.isArray(value) ? value : { result: value };
  return { content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: structured, isError: false };
}

function handleOne(msg: unknown, origin: string): RpcOut | null {
  if (typeof msg !== 'object' || msg === null || Array.isArray(msg)) return rpcError(null, -32600, 'Invalid Request');
  const m = msg as Record<string, unknown>;
  const id = (typeof m.id === 'string' || typeof m.id === 'number' ? m.id : null) as Id;
  const isNotification = !('id' in m);
  if (m.jsonrpc !== '2.0' || typeof m.method !== 'string') return isNotification ? null : rpcError(id, -32600, 'Invalid Request');
  const params = (typeof m.params === 'object' && m.params !== null ? m.params : {}) as Record<string, unknown>;
  if (isNotification) return null;
  switch (m.method) {
    case 'server/discover':
      return {
        jsonrpc: '2.0',
        id,
        result: {
          supportedVersions: SUPPORTED,
          capabilities: { tools: { listChanged: false } },
          serverInfo: SERVER_INFO,
          instructions: INSTRUCTIONS,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        },
      };
    case 'initialize': {
      const asked = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const version = SUPPORTED.includes(asked) && asked !== '2026-07-28' ? asked : '2025-11-25';
      return { jsonrpc: '2.0', id, result: { protocolVersion: version, capabilities: { tools: { listChanged: false } }, serverInfo: SERVER_INFO, instructions: INSTRUCTIONS } };
    }
    case 'ping':
      return { jsonrpc: '2.0', id, result: {} };
    case 'tools/list':
      return { jsonrpc: '2.0', id, result: { tools: TOOL_LIST } };
    case 'resources/list':
      return { jsonrpc: '2.0', id, result: { resources: [] } };
    case 'prompts/list':
      return { jsonrpc: '2.0', id, result: { prompts: [] } };
    case 'tools/call': {
      const name = params.name;
      const tool = typeof name === 'string' ? TOOLS_BY_NAME.get(name) : undefined;
      if (tool === undefined) return rpcError(id, -32602, `Unknown tool: ${String(name)}`);
      const args = (typeof params.arguments === 'object' && params.arguments !== null && !Array.isArray(params.arguments) ? params.arguments : {}) as Record<string, unknown>;
      const missing = tool.required.filter((k) => args[k] === undefined || args[k] === null || args[k] === '');
      if (missing.length > 0) return { jsonrpc: '2.0', id, result: toolResult(`missing required argument(s): ${missing.join(', ')}`, true) };
      try {
        return { jsonrpc: '2.0', id, result: toolResult(tool.run(args, origin), false) };
      } catch (err) {
        if (err instanceof ToolError || err instanceof HttpError) return { jsonrpc: '2.0', id, result: toolResult(err.message, true) };
        return { jsonrpc: '2.0', id, result: toolResult(`internal error: ${(err as Error).message}`, true) };
      }
    }
    default:
      return rpcError(id, -32601, `Method not found: ${m.method}`);
  }
}

/** One POST /mcp body -> HTTP status and the JSON reply (null body = 202 Accepted). */
export function handleMcp(raw: string, origin: string): { status: number; body: unknown } {
  let msg: unknown;
  try {
    msg = JSON.parse(raw);
  } catch {
    return { status: 400, body: rpcError(null, -32700, 'Parse error') };
  }
  if (Array.isArray(msg)) {
    if (msg.length === 0) return { status: 400, body: rpcError(null, -32600, 'Invalid Request') };
    const out = msg.map((x) => handleOne(x, origin)).filter((x): x is RpcOut => x !== null);
    return out.length === 0 ? { status: 202, body: null } : { status: 200, body: out };
  }
  const out = handleOne(msg, origin);
  return out === null ? { status: 202, body: null } : { status: 200, body: out };
}