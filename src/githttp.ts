// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 and v0) and receive-pack (v0).

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  DELIM, FLUSH, ZERO, isOid, parseTagTarget, parseTree, pkt, readPack, readPackets, sideband, writePack,
  type GitObject, type Packet,
} from './gitobj.ts';
import { commitOf, getRepo, isAncestor, peelToCommit, setRef, type Repo } from './store.ts';

const AGENT = 'agent=forge/1.0';

function send(res: ServerResponse, status: number, type: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': type, 'content-length': body.length, 'cache-control': 'no-cache' });
  res.end(body);
}

function notFound(res: ServerResponse): void {
  const body = Buffer.from('{"message":"Repository not found"}\n');
  send(res, 404, 'application/json; charset=utf-8', body);
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** Refs in advertisement order: sorted names, HEAD handled by the caller. */
function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

function peeled(repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'tag') return null;
  return peelToCommit(repo, oid) ?? parseTagTarget(obj.data).object;
}

function v0Advertisement(repo: Repo, service: string, caps: string): Buffer {
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const lines: string[] = [];
  const head = repo.refs.get(headTarget(repo));
  if (head !== undefined && service === 'git-upload-pack') lines.push(`${head} HEAD`);
  for (const [name, oid] of sortedRefs(repo)) {
    lines.push(`${oid} ${name}`);
    if (service === 'git-upload-pack') {
      const p = peeled(repo, oid);
      if (p !== null) lines.push(`${p} ${name}^{}`);
    }
  }
  if (lines.length === 0) lines.push(`${ZERO} capabilities^{}`);
  lines.forEach((line, i) => parts.push(pkt(i === 0 ? `${line}\0${caps}\n` : `${line}\n`)));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

const V2_ADVERT = Buffer.concat([
  pkt('version 2\n'),
  pkt(`${AGENT}\n`),
  pkt('ls-refs=unborn\n'),
  pkt('fetch=shallow\n'),
  pkt('server-option\n'),
  pkt('object-format=sha1\n'),
  FLUSH,
]);

export function handleInfoRefs(req: IncomingMessage, res: ServerResponse, owner: string, name: string, service: string | null): void {
  const repo = getRepo(owner, name);
  if (repo === undefined) return notFound(res);
  if (service === 'git-upload-pack') {
    const proto = String(req.headers['git-protocol'] ?? '');
    if (/version=2/.test(proto)) return send(res, 200, 'application/x-git-upload-pack-advertisement', V2_ADVERT);
    const caps = `multi_ack_detailed side-band-64k ofs-delta shallow no-progress thin-pack include-tag symref=HEAD:${headTarget(repo)} object-format=sha1 ${AGENT}`;
    return send(res, 200, 'application/x-git-upload-pack-advertisement', v0Advertisement(repo, service, caps));
  }
  if (service === 'git-receive-pack') {
    const caps = `report-status side-band-64k delete-refs ofs-delta quiet object-format=sha1 ${AGENT}`;
    return send(res, 200, 'application/x-git-receive-pack-advertisement', v0Advertisement(repo, service, caps));
  }
  const body = Buffer.from('{"message":"Unsupported service"}\n');
  send(res, 403, 'application/json; charset=utf-8', body);
}

// ------------------------------------------------------------------ object selection

/** Every object reachable from `roots` (commits through parents unless cut), trees, blobs, tag targets. */
function closure(repo: Repo, roots: string[], cut: Set<string>, commitLimit: Map<string, number> | null): Set<string> {
  const seen = new Set<string>();
  const stack = roots.slice();
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    seen.add(oid);
    if (obj.type === 'commit') {
      const c = commitOf(repo, oid);
      if (c === undefined) continue;
      stack.push(c.tree);
      if (cut.has(oid)) continue;
      if (commitLimit !== null && !commitLimit.has(oid)) continue;
      for (const p of c.parents) if (commitLimit === null || commitLimit.has(p)) stack.push(p);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) if (e.mode !== '160000') stack.push(e.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTagTarget(obj.data).object);
    }
  }
  return seen;
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
}

function planFetch(repo: Repo, args: FetchArgs): FetchPlan {
  const clientShallow = new Set(args.shallow.filter((o) => repo.objects.has(o)));
  let limit: Map<string, number> | null = null;
  const shallowOut: string[] = [];
  const unshallow: string[] = [];
  if (args.depth !== null && args.depth > 0) {
    // Commits within `depth` generations of the wants; the deepest with parents become the boundary.
    limit = new Map();
    let frontier: string[] = [];
    for (const w of args.wants) {
      const c = peelToCommit(repo, w);
      if (c !== undefined && !limit.has(c)) {
        limit.set(c, 1);
        frontier.push(c);
      }
    }
    for (let d = 1; d < args.depth; d += 1) {
      const next: string[] = [];
      for (const oid of frontier) {
        const c = commitOf(repo, oid);
        if (c === undefined) continue;
        for (const p of c.parents) if (!limit.has(p) && repo.objects.has(p)) {
          limit.set(p, d + 1);
          next.push(p);
        }
      }
      frontier = next;
    }
    for (const [oid, d] of limit) {
      const c = commitOf(repo, oid);
      if (c === undefined) continue;
      const hasParents = c.parents.some((p) => repo.objects.has(p));
      if (d === args.depth && hasParents) {
        if (!clientShallow.has(oid)) shallowOut.push(oid);
      } else if (clientShallow.has(oid) && hasParents) {
        unshallow.push(oid);
      }
    }
  }
  const haves = args.haves.filter((h) => repo.objects.has(h));
  const have = haves.length === 0 ? new Set<string>() : closure(repo, haves, clientShallow, null);
  const want = closure(repo, args.wants, new Set(), limit);
  const objects: GitObject[] = [];
  // Commits first, then trees, blobs and tags: the order git itself writes.
  const order: Record<string, number> = { commit: 0, tag: 1, tree: 2, blob: 3 };
  const picked: Array<[string, GitObject]> = [];
  for (const oid of want) {
    if (have.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj !== undefined) picked.push([oid, obj]);
  }
  picked.sort((a, b) => order[a[1].type] - order[b[1].type]);
  for (const [, obj] of picked) objects.push(obj);
  return { objects, shallow: shallowOut.sort(), unshallow: unshallow.sort() };
}

function cachedPack(repo: Repo, args: FetchArgs): { pack: Buffer; plan: FetchPlan } {
  const k = `${args.wants.join(',')}|${args.haves.join(',')}|${args.shallow.join(',')}|${args.depth ?? ''}`;
  const hit = repo.packCache.get(k);
  const plan = planFetch(repo, args);
  if (hit !== undefined) return { pack: hit, plan };
  const pack = writePack(plan.objects);
  if (repo.packCache.size > 64) repo.packCache.clear();
  repo.packCache.set(k, pack);
  return { pack, plan };
}

// A cheaper path for the common case: repeated identical shallow clones. The plan itself is cached too.
const planCache = new WeakMap<Repo, Map<string, { version: number; pack: Buffer; plan: FetchPlan }>>();
function fetchPack(repo: Repo, args: FetchArgs): { pack: Buffer; plan: FetchPlan } {
  const k = `${args.wants.join(',')}|${args.haves.join(',')}|${args.shallow.join(',')}|${args.depth ?? ''}`;
  let m = planCache.get(repo);
  if (m === undefined) {
    m = new Map();
    planCache.set(repo, m);
  }
  const hit = m.get(k);
  if (hit !== undefined && hit.version === repo.refVersion) return hit;
  const fresh = cachedPack(repo, args);
  if (m.size > 64) m.clear();
  m.set(k, { version: repo.refVersion, pack: fresh.pack, plan: fresh.plan });
  return fresh;
}

// ------------------------------------------------------------------ upload-pack

function lines(packets: Packet[]): string[] {
  const out: string[] = [];
  for (const p of packets) if (p.kind === 'data') out.push(p.payload.toString('utf8').replace(/\n$/, ''));
  return out;
}

function lsRefs(repo: Repo, args: string[]): Buffer {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice(11));
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const parts: Buffer[] = [];
  const target = headTarget(repo);
  const head = repo.refs.get(target);
  if (keep('HEAD')) {
    if (head !== undefined) parts.push(pkt(`${head} HEAD${symrefs ? ` symref-target:${target}` : ''}\n`));
    else if (unborn) parts.push(pkt(`unborn HEAD${symrefs ? ` symref-target:${target}` : ''}\n`));
  }
  for (const [name, oid] of sortedRefs(repo)) {
    if (!keep(name)) continue;
    let line = `${oid} ${name}`;
    if (peel) {
      const p = peeled(repo, oid);
      if (p !== null) line += ` peeled:${p}`;
    }
    parts.push(pkt(`${line}\n`));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function parseFetchArgs(args: string[]): FetchArgs & { done: boolean } {
  const out: FetchArgs & { done: boolean } = { wants: [], haves: [], shallow: [], depth: null, done: false };
  for (const a of args) {
    const sp = a.indexOf(' ');
    const word = sp < 0 ? a : a.slice(0, sp);
    const val = sp < 0 ? '' : a.slice(sp + 1).split(' ')[0];
    if (word === 'want' && isOid(val)) out.wants.push(val);
    else if (word === 'have' && isOid(val)) out.haves.push(val);
    else if (word === 'shallow' && isOid(val)) out.shallow.push(val);
    else if (word === 'deepen') out.depth = Number(val) || null;
    else if (word === 'done') out.done = true;
  }
  out.wants = [...new Set(out.wants)];
  return out;
}

function fetchV2(repo: Repo, args: string[]): Buffer {
  const fa = parseFetchArgs(args);
  const parts: Buffer[] = [];
  if (!fa.done && fa.haves.length > 0) {
    // Negotiation round: acknowledge what we hold, say ready, and send the pack anyway.
    parts.push(pkt('acknowledgments\n'));
    const common = fa.haves.filter((h) => repo.objects.has(h));
    if (common.length === 0) parts.push(pkt('NAK\n'));
    for (const h of common) parts.push(pkt(`ACK ${h}\n`));
    parts.push(pkt('ready\n'), DELIM);
  }
  const { pack, plan } = fetchPack(repo, fa);
  if (fa.depth !== null || fa.shallow.length > 0) {
    parts.push(pkt('shallow-info\n'));
    for (const s of plan.shallow) parts.push(pkt(`shallow ${s}\n`));
    for (const u of plan.unshallow) parts.push(pkt(`unshallow ${u}\n`));
    parts.push(DELIM);
  }
  parts.push(pkt('packfile\n'));
  sideband(1, pack, parts);
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function uploadPackV0(repo: Repo, body: Buffer): Buffer {
  const first = readPackets(body, 0, true);
  const wantLines = lines(first.packets);
  const second = first.next < body.length ? readPackets(body, first.next, false) : { packets: [] as Packet[], next: body.length };
  const haveLines = lines(second.packets);
  const fa = parseFetchArgs([...wantLines, ...haveLines]);
  const parts: Buffer[] = [];
  if (fa.wants.length === 0) return FLUSH;
  const { pack, plan } = fetchPack(repo, fa);
  if (fa.depth !== null) {
    for (const s of plan.shallow) parts.push(pkt(`shallow ${s}\n`));
    for (const u of plan.unshallow) parts.push(pkt(`unshallow ${u}\n`));
    parts.push(FLUSH);
  }
  const common = fa.haves.filter((h) => repo.objects.has(h));
  const multiAck = /multi_ack/.test(wantLines[0] ?? '');
  if (common.length > 0 && multiAck) {
    for (const h of common) parts.push(pkt(`ACK ${h} common\n`));
    parts.push(pkt(`ACK ${common[common.length - 1]}\n`));
  } else if (common.length > 0) {
    parts.push(pkt(`ACK ${common[0]}\n`));
  } else {
    parts.push(pkt('NAK\n'));
  }
  sideband(1, pack, parts);
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

export function handleUploadPack(req: IncomingMessage, res: ServerResponse, owner: string, name: string, body: Buffer): void {
  const repo = getRepo(owner, name);
  if (repo === undefined) return notFound(res);
  const type = 'application/x-git-upload-pack-result';
  try {
    const v2 = /version=2/.test(String(req.headers['git-protocol'] ?? '')) || body.subarray(4, 12).toString('latin1') === 'command=';
    if (!v2) return send(res, 200, type, uploadPackV0(repo, body));
    const { packets } = readPackets(body, 0, false);
    let command = '';
    const args: string[] = [];
    let inArgs = false;
    for (const p of packets) {
      if (p.kind === 'delim') {
        inArgs = true;
        continue;
      }
      if (p.kind === 'flush') break;
      if (p.kind !== 'data') continue;
      const line = p.payload.toString('utf8').replace(/\n$/, '');
      if (!inArgs) {
        if (line.startsWith('command=')) command = line.slice(8);
      } else {
        args.push(line);
      }
    }
    if (command === 'ls-refs') return send(res, 200, type, lsRefs(repo, args));
    if (command === 'fetch') return send(res, 200, type, fetchV2(repo, args));
    return send(res, 400, 'application/json; charset=utf-8', Buffer.from(`{"message":"unknown command ${JSON.stringify(command).slice(1, -1)}"}\n`));
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return send(res, 400, 'application/json; charset=utf-8', Buffer.from(`${JSON.stringify({ message: `bad upload-pack request: ${msg}` })}\n`));
  }
}

// ------------------------------------------------------------------ receive-pack

interface Command {
  old: string;
  neu: string;
  ref: string;
}

export function handleReceivePack(res: ServerResponse, owner: string, name: string, body: Buffer): void {
  const repo = getRepo(owner, name);
  if (repo === undefined) return notFound(res);
  let commands: Command[] = [];
  let caps = new Set<string>();
  let packStart = 0;
  try {
    const { packets, next } = readPackets(body, 0, true);
    packStart = next;
    for (const p of packets) {
      if (p.kind !== 'data') continue;
      let line = p.payload.toString('utf8').replace(/\n$/, '');
      const nul = line.indexOf('\0');
      if (nul >= 0) {
        caps = new Set(line.slice(nul + 1).split(' '));
        line = line.slice(0, nul);
      }
      const [old, neu, ref] = line.split(' ');
      if (!isOid(old) || !isOid(neu) || ref === undefined || !ref.startsWith('refs/')) throw new Error('bad command');
      commands.push({ old, neu, ref });
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return send(res, 400, 'application/json; charset=utf-8', Buffer.from(`${JSON.stringify({ message: `bad receive-pack request: ${msg}` })}\n`));
  }
  let unpack = 'ok';
  try {
    const pack = body.subarray(packStart);
    if (pack.length > 0) {
      const objs = readPack(pack, (oid) => repo.objects.get(oid));
      for (const [oid, obj] of objs) if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
    }
  } catch (err) {
    unpack = err instanceof Error ? err.message.replace(/\n/g, ' ') : 'error';
  }
  const report: string[] = [`unpack ${unpack}\n`];
  for (const c of commands) {
    const current = repo.refs.get(c.ref) ?? ZERO;
    let ng: string | null = null;
    if (unpack !== 'ok') ng = 'unpacker error';
    else if (c.neu !== ZERO && !repo.objects.has(c.neu)) ng = 'missing necessary objects';
    else if (current !== c.old) ng = c.old === ZERO ? 'already exists' : current === ZERO ? 'no such ref' : 'fetch first';
    else if (c.neu !== ZERO && c.old !== ZERO) {
      const oldC = peelToCommit(repo, c.old);
      const newC = peelToCommit(repo, c.neu);
      const ff = c.ref.startsWith('refs/tags/')
        ? false
        : oldC !== undefined && newC !== undefined && isAncestor(repo, oldC, newC);
      if (!ff) ng = 'non-fast-forward';
    }
    if (ng !== null) report.push(`ng ${c.ref} ${ng}\n`);
    else {
      setRef(repo, c.ref, c.neu === ZERO ? null : c.neu);
      report.push(`ok ${c.ref}\n`);
    }
  }
  const inner = Buffer.concat([...report.map((l) => pkt(l)), FLUSH]);
  const type = 'application/x-git-receive-pack-result';
  if (caps.has('side-band-64k') || caps.has('side-band')) {
    const parts: Buffer[] = [];
    sideband(1, inner, parts);
    parts.push(FLUSH);
    return send(res, 200, type, Buffer.concat(parts));
  }
  if (!caps.has('report-status') && !caps.has('report-status-v2')) return send(res, 200, type, Buffer.alloc(0));
  send(res, 200, type, inner);
}
