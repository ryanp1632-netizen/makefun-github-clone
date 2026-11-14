// The forge's whole state, in memory: repositories with their git objects and refs, and the
// issues, labels, comments and pull requests around them. Every surface (git, REST, GraphQL, MCP)
// reads and writes through the functions here, so they always agree.

import { VIEWER } from './identity.ts';
import {
  ZERO, buildCommit, buildTree, hashObject, parseCommit, parseTagTarget, parseTree,
  type GitObject, type ObjType, type TreeEntry,
} from './gitobj.ts';

// ------------------------------------------------------------------ the logical clock

// Times come from a counter, never the wall clock: every created object gets the next second after
// a fixed epoch, so ordering by creation time is total and two runs produce identical data.
const EPOCH_MS = Date.UTC(2026, 0, 1);
let tick = 0;
export function nextTime(): { iso: string; sec: number } {
  tick += 1;
  const ms = EPOCH_MS + tick * 1000;
  return { iso: new Date(ms).toISOString().replace('.000Z', 'Z'), sec: ms / 1000 };
}

let idCounter = 0;
export function nextId(): number {
  idCounter += 1;
  return 100000 + idCounter;
}

export function globalId(kind: string, id: number): string {
  return `${kind}_${Buffer.from(`${kind}:${id}`).toString('base64url')}`;
}

export function decodeGlobalId(gid: string): { kind: string; id: number } | null {
  const us = gid.indexOf('_');
  if (us < 1) return null;
  try {
    const text = Buffer.from(gid.slice(us + 1), 'base64url').toString('utf8');
    const m = /^([A-Za-z]+):(\d+)$/.exec(text);
    if (!m || m[1] !== gid.slice(0, us)) return null;
    return { kind: m[1], id: Number(m[2]) };
  } catch {
    return null;
  }
}

// ------------------------------------------------------------------ the model

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  id: number;
  body: string;
  created: string;
  issue: Issue;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: Comment[];
  created: string;
  updated: string;
  closed: string | null;
  seq: number;
  pull: Pull | null;
  repo: Repo;
  /** Cached JSON of the REST issue view; cleared on every change. */
  json?: string;
}

export interface Pull {
  issue: Issue;
  id: number;
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  created: string;
  updated: string;
  pushed: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  /** Bumped on every ref change: keys the pack cache. */
  refVersion: number;
  packCache: Map<string, Buffer>;
  labels: Map<string, Label>;
  issues: Issue[];
  byNumber: Map<number, Issue>;
  nextNumber: number;
  json?: string;
}

const repos = new Map<string, Repo>();
const reposById = new Map<number, Repo>();
export const issuesById = new Map<number, Issue>();
export const commentsById = new Map<number, Comment>();

function key(owner: string, name: string): string {
  return `${owner.toLowerCase()}/${name.toLowerCase()}`;
}

export function getRepo(owner: string, name: string): Repo | undefined {
  return repos.get(key(owner, name));
}

export function repoById(id: number): Repo | undefined {
  return reposById.get(id);
}

export function listRepos(owner: string): Repo[] {
  const out: Repo[] = [];
  for (const r of repos.values()) if (r.owner.toLowerCase() === owner.toLowerCase()) out.push(r);
  return out;
}

export function validRepoName(name: unknown): name is string {
  return typeof name === 'string' && /^[A-Za-z0-9._-]{1,100}$/.test(name) && name !== '.' && name !== '..' && !name.endsWith('.git');
}

export function createRepo(name: string, opts: { description?: string; private?: boolean; autoInit?: boolean } = {}): Repo {
  const t = nextTime();
  const id = nextId();
  const repo: Repo = {
    id,
    owner: VIEWER.login,
    name,
    description: opts.description ?? '',
    private: opts.private === true,
    defaultBranch: 'main',
    created: t.iso,
    updated: t.iso,
    pushed: t.iso,
    objects: new Map(),
    refs: new Map(),
    refVersion: 0,
    packCache: new Map(),
    labels: new Map(),
    issues: [],
    byNumber: new Map(),
    nextNumber: 1,
  };
  repos.set(key(VIEWER.login, name), repo);
  reposById.set(id, repo);
  if (opts.autoInit) {
    commitFiles(repo, 'main', [{ path: 'README.md', content: Buffer.from(`# ${name}\n`) }], 'Initial commit');
  }
  return repo;
}

export function deleteRepo(repo: Repo): void {
  repos.delete(key(repo.owner, repo.name));
  reposById.delete(repo.id);
  for (const issue of repo.issues) {
    issuesById.delete(issue.id);
    for (const c of issue.comments) commentsById.delete(c.id);
  }
}

export function touch(repo: Repo): void {
  repo.updated = nextTime().iso;
  repo.json = undefined;
}

// ------------------------------------------------------------------ objects and refs

export function putObject(repo: Repo, type: ObjType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

export function setRef(repo: Repo, ref: string, oid: string | null): void {
  if (oid === null) repo.refs.delete(ref);
  else repo.refs.set(ref, oid);
  repo.refVersion += 1;
  repo.packCache.clear();
  repo.pushed = nextTime().iso;
  repo.json = undefined;
}

/** The commit a branch name, full ref or sha names, or undefined. */
export function resolve(repo: Repo, rev: string | undefined): string | undefined {
  if (rev === undefined || rev === '') return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  const direct = repo.refs.get(rev) ?? repo.refs.get(`refs/heads/${rev}`) ?? repo.refs.get(`refs/tags/${rev}`);
  if (direct !== undefined) return peelToCommit(repo, direct);
  if (/^[0-9a-f]{40}$/.test(rev) && repo.objects.has(rev)) return peelToCommit(repo, rev);
  return undefined;
}

export function peelToCommit(repo: Repo, oid: string): string | undefined {
  let cur = oid;
  for (let i = 0; i < 10; i += 1) {
    const obj = repo.objects.get(cur);
    if (obj === undefined) return undefined;
    if (obj.type === 'commit') return cur;
    if (obj.type !== 'tag') return undefined;
    cur = parseTagTarget(obj.data).object;
  }
  return undefined;
}

const commitCache = new WeakMap<GitObject, ReturnType<typeof parseCommit>>();
export function commitOf(repo: Repo, oid: string): ReturnType<typeof parseCommit> | undefined {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'commit') return undefined;
  let c = commitCache.get(obj);
  if (c === undefined) {
    c = parseCommit(obj.data);
    commitCache.set(obj, c);
  }
  return c;
}

/** Whether `ancestor` is reachable from `descendant` (inclusive). */
export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const cur = stack.pop() as string;
    if (cur === ancestor) return true;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const c = commitOf(repo, cur);
    if (c !== undefined) for (const p of c.parents) stack.push(p);
  }
  return false;
}

export function mergeBase(repo: Repo, a: string, b: string): string | undefined {
  const ancestors = new Set<string>();
  const stack = [a];
  while (stack.length > 0) {
    const cur = stack.pop() as string;
    if (ancestors.has(cur)) continue;
    ancestors.add(cur);
    const c = commitOf(repo, cur);
    if (c !== undefined) for (const p of c.parents) stack.push(p);
  }
  const queue = [b];
  const seen = new Set<string>();
  while (queue.length > 0) {
    const cur = queue.shift() as string;
    if (ancestors.has(cur)) return cur;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const c = commitOf(repo, cur);
    if (c !== undefined) for (const p of c.parents) queue.push(p);
  }
  return undefined;
}

/** Commits reachable from `tip`, newest first by committer order of discovery (breadth-first). */
export function history(repo: Repo, tip: string, limit: number): string[] {
  const out: string[] = [];
  const seen = new Set<string>([tip]);
  const queue = [tip];
  while (queue.length > 0 && out.length < limit) {
    const cur = queue.shift() as string;
    out.push(cur);
    const c = commitOf(repo, cur);
    if (c === undefined) continue;
    for (const p of c.parents) if (!seen.has(p)) {
      seen.add(p);
      queue.push(p);
    }
  }
  return out;
}

// ------------------------------------------------------------------ trees as flat path maps

export interface FileEntry {
  mode: string;
  oid: string;
}

export function flattenTree(repo: Repo, treeOid: string, prefix = '', out = new Map<string, FileEntry>()): Map<string, FileEntry> {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') flattenTree(repo, e.oid, path, out);
    else out.set(path, { mode: e.mode, oid: e.oid });
  }
  return out;
}

export function buildTreeFromMap(repo: Repo, files: Map<string, FileEntry>): string {
  interface Dir { files: Map<string, FileEntry>; dirs: Map<string, Dir> }
  const root: Dir = { files: new Map(), dirs: new Map() };
  for (const [path, entry] of files) {
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      let next = node.dirs.get(parts[i]);
      if (next === undefined) {
        next = { files: new Map(), dirs: new Map() };
        node.dirs.set(parts[i], next);
      }
      node = next;
    }
    node.files.set(parts[parts.length - 1], entry);
  }
  const write = (dir: Dir): string => {
    const entries: TreeEntry[] = [];
    for (const [name, e] of dir.files) entries.push({ mode: e.mode, name, oid: e.oid });
    for (const [name, d] of dir.dirs) entries.push({ mode: '40000', name, oid: write(d) });
    return putObject(repo, 'tree', buildTree(entries));
  };
  return write(root);
}

export function treeOfCommit(repo: Repo, commit: string | undefined): Map<string, FileEntry> {
  if (commit === undefined) return new Map();
  const c = commitOf(repo, commit);
  return c === undefined ? new Map() : flattenTree(repo, c.tree);
}

/** A tree entry at `path` under a commit: a file, a directory listing, or null. */
export function lookupPath(repo: Repo, commit: string, path: string): { kind: 'file'; oid: string; mode: string } | { kind: 'dir'; oid: string; entries: TreeEntry[] } | null {
  const c = commitOf(repo, commit);
  if (c === undefined) return null;
  let treeOid = c.tree;
  const parts = path.split('/').filter((p) => p !== '');
  if (parts.length === 0) {
    const t = repo.objects.get(treeOid);
    return t === undefined ? null : { kind: 'dir', oid: treeOid, entries: parseTree(t.data) };
  }
  for (let i = 0; i < parts.length; i += 1) {
    const t = repo.objects.get(treeOid);
    if (t === undefined || t.type !== 'tree') return null;
    const e = parseTree(t.data).find((x) => x.name === parts[i]);
    if (e === undefined) return null;
    if (i === parts.length - 1) {
      if (e.mode === '40000') {
        const sub = repo.objects.get(e.oid);
        return sub === undefined ? null : { kind: 'dir', oid: e.oid, entries: parseTree(sub.data) };
      }
      return { kind: 'file', oid: e.oid, mode: e.mode };
    }
    if (e.mode !== '40000') return null;
    treeOid = e.oid;
  }
  return null;
}

function ident(): string {
  return `${VIEWER.name} <${VIEWER.email}> ${nextTime().sec} +0000`;
}

export function makeCommit(repo: Repo, tree: string, parents: string[], message: string): string {
  const who = ident();
  return putObject(repo, 'commit', buildCommit({ tree, parents, author: who, committer: who, message: message.endsWith('\n') ? message : `${message}\n` }));
}

/** Commit file changes (content null deletes) on a branch; creates the branch when unborn. */
export function commitFiles(repo: Repo, branch: string, changes: Array<{ path: string; content: Buffer | null }>, message: string): { commit: string; blobs: Map<string, string> } {
  const ref = `refs/heads/${branch}`;
  const parent = repo.refs.get(ref);
  const files = treeOfCommit(repo, parent);
  const blobs = new Map<string, string>();
  for (const ch of changes) {
    if (ch.content === null) files.delete(ch.path);
    else {
      const oid = putObject(repo, 'blob', ch.content);
      blobs.set(ch.path, oid);
      files.set(ch.path, { mode: '100644', oid });
    }
  }
  const tree = buildTreeFromMap(repo, files);
  const commit = makeCommit(repo, tree, parent === undefined ? [] : [parent], message);
  setRef(repo, ref, commit);
  return { commit, blobs };
}

// ------------------------------------------------------------------ labels, issues, comments

export function addLabel(repo: Repo, name: string, color: string, description: string): Label {
  const label: Label = { id: nextId(), name, color, description };
  repo.labels.set(name.toLowerCase(), label);
  return label;
}

let issueSeq = 0;
export function createIssue(repo: Repo, title: string, body: string, labels: Label[]): Issue {
  const t = nextTime().iso;
  issueSeq += 1;
  const issue: Issue = {
    id: nextId(),
    number: repo.nextNumber++,
    title,
    body,
    state: 'open',
    labels,
    comments: [],
    created: t,
    updated: t,
    closed: null,
    seq: issueSeq,
    pull: null,
    repo,
  };
  repo.issues.push(issue);
  repo.byNumber.set(issue.number, issue);
  issuesById.set(issue.id, issue);
  repo.json = undefined;
  return issue;
}

export function setIssueState(issue: Issue, state: 'open' | 'closed'): void {
  if (issue.state !== state) {
    issue.state = state;
    issue.closed = state === 'closed' ? nextTime().iso : null;
  }
  issue.updated = nextTime().iso;
  issue.json = undefined;
  issue.repo.json = undefined;
}

export function addComment(issue: Issue, body: string): Comment {
  const c: Comment = { id: nextId(), body, created: nextTime().iso, issue };
  issue.comments.push(c);
  commentsById.set(c.id, c);
  issue.updated = c.created;
  issue.json = undefined;
  return c;
}

// ------------------------------------------------------------------ pull requests

export function createPull(repo: Repo, title: string, body: string, head: string, base: string): Pull {
  const issue = createIssue(repo, title, body, []);
  const pull: Pull = {
    issue,
    id: nextId(),
    head,
    base,
    headSha: repo.refs.get(`refs/heads/${head}`) ?? ZERO,
    baseSha: repo.refs.get(`refs/heads/${base}`) ?? ZERO,
    merged: false,
    mergedAt: null,
    mergeSha: null,
  };
  issue.pull = pull;
  return pull;
}

/** The pull's live head and base shas while open; frozen once closed. */
export function pullShas(pull: Pull): { head: string; base: string } {
  if (pull.issue.state === 'open') {
    const repo = pull.issue.repo;
    pull.headSha = repo.refs.get(`refs/heads/${pull.head}`) ?? pull.headSha;
    pull.baseSha = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  }
  return { head: pull.headSha, base: pull.baseSha };
}

export interface ChangedFile {
  path: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  oid: string;
}

function lineCounts(oldText: string, newText: string): { additions: number; deletions: number } {
  const a = oldText === '' ? [] : oldText.split('\n');
  const b = newText === '' ? [] : newText.split('\n');
  const counts = new Map<string, number>();
  for (const line of a) counts.set(line, (counts.get(line) ?? 0) + 1);
  let common = 0;
  for (const line of b) {
    const n = counts.get(line) ?? 0;
    if (n > 0) {
      counts.set(line, n - 1);
      common += 1;
    }
  }
  return { additions: b.length - common, deletions: a.length - common };
}

/** The files a pull request changes: head's tree against the merge base's. */
export function pullFiles(pull: Pull): ChangedFile[] {
  const repo = pull.issue.repo;
  const { head, base } = pullShas(pull);
  const mb = pull.merged ? base : (mergeBase(repo, base, head) ?? base);
  const before = treeOfCommit(repo, pull.merged ? (commitOf(repo, pull.mergeSha ?? '')?.parents[0] ?? mb) : mb);
  const after = treeOfCommit(repo, pull.merged ? (pull.mergeSha ?? head) : head);
  const out: ChangedFile[] = [];
  const text = (oid: string | undefined): string => (oid === undefined ? '' : repo.objects.get(oid)?.data.toString('utf8') ?? '');
  for (const [path, e] of after) {
    const old = before.get(path);
    if (old === undefined) out.push({ path, status: 'added', oid: e.oid, ...lineCounts('', text(e.oid)) });
    else if (old.oid !== e.oid) out.push({ path, status: 'modified', oid: e.oid, ...lineCounts(text(old.oid), text(e.oid)) });
  }
  for (const [path, e] of before) if (!after.has(path)) out.push({ path, status: 'removed', oid: e.oid, ...lineCounts(text(e.oid), '') });
  out.sort((x, y) => (x.path < y.path ? -1 : x.path > y.path ? 1 : 0));
  return out;
}

/** The squash result's file map, or null when head and base changed a path differently. */
function squashFiles(repo: Repo, base: string, head: string): Map<string, FileEntry> | null {
  const mb = mergeBase(repo, base, head);
  const m = treeOfCommit(repo, mb);
  const b = treeOfCommit(repo, base);
  const h = treeOfCommit(repo, head);
  const out = new Map<string, FileEntry>();
  const paths = new Set<string>([...m.keys(), ...b.keys(), ...h.keys()]);
  const same = (x: FileEntry | undefined, y: FileEntry | undefined): boolean => (x === undefined ? y === undefined : y !== undefined && x.oid === y.oid && x.mode === y.mode);
  for (const p of paths) {
    const mv = m.get(p);
    const bv = b.get(p);
    const hv = h.get(p);
    let take: FileEntry | undefined;
    if (same(hv, mv)) take = bv;
    else if (same(bv, mv)) take = hv;
    else if (same(bv, hv)) take = bv;
    else return null;
    if (take !== undefined) out.set(p, take);
  }
  return out;
}

export function isMergeable(pull: Pull): boolean {
  if (pull.issue.state !== 'open') return false;
  const { head, base } = pullShas(pull);
  if (!pull.issue.repo.objects.has(head) || !pull.issue.repo.objects.has(base)) return false;
  return squashFiles(pull.issue.repo, base, head) !== null;
}

export type MergeResult = { ok: true; sha: string } | { ok: false; status: number; message: string };

export function mergePull(pull: Pull, method: string, title?: string, message?: string): MergeResult {
  const repo = pull.issue.repo;
  if (pull.merged) return { ok: false, status: 405, message: 'Pull Request is not mergeable: it is already merged' };
  if (pull.issue.state !== 'open') return { ok: false, status: 405, message: 'Pull Request is not mergeable: it is closed' };
  const { head, base } = pullShas(pull);
  if (!repo.objects.has(head) || !repo.objects.has(base)) return { ok: false, status: 405, message: 'Pull Request is not mergeable' };
  const files = squashFiles(repo, base, head);
  if (files === null) return { ok: false, status: 405, message: 'Pull Request is not mergeable: merge conflict' };
  const tree = buildTreeFromMap(repo, files);
  const subject = title !== undefined && title !== '' ? title : `${pull.issue.title} (#${pull.issue.number})`;
  const body = message !== undefined && message !== '' ? `${subject}\n\n${message}` : subject;
  const parents = method === 'merge' ? [base, head] : [base];
  const sha = makeCommit(repo, tree, parents, body);
  setRef(repo, `refs/heads/${pull.base}`, sha);
  pull.merged = true;
  pull.mergedAt = nextTime().iso;
  pull.mergeSha = sha;
  pull.headSha = head;
  pull.baseSha = base;
  setIssueState(pull.issue, 'closed');
  pull.issue.closed = pull.mergedAt;
  return { ok: true, sha };
}

export { ZERO };
