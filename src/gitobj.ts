// Git objects, trees, commits, pkt-lines and packfiles: everything below the protocol, in memory.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync, constants as zc } from 'node:zlib';

export const ZERO = '0000000000000000000000000000000000000000';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
  /** Cached zlib stream of `data`, filled the first time the object goes into a pack. */
  z?: Buffer;
}

const TYPE_NUM: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const NUM_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function hashObject(type: ObjType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(`${type} ${data.length}\0`);
  h.update(data);
  return h.digest('hex');
}

export function isOid(s: unknown): s is string {
  return typeof s === 'string' && /^[0-9a-f]{40}$/.test(s);
}

// ------------------------------------------------------------------ trees

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const nul = data.indexOf(0, sp);
    if (sp < 0 || nul < 0 || nul + 21 > data.length) throw new Error('bad tree');
    out.push({
      mode: data.toString('latin1', i, sp),
      name: data.toString('utf8', sp + 1, nul),
      oid: data.toString('hex', nul + 1, nul + 21),
    });
    i = nul + 21;
  }
  return out;
}

function sortKey(e: TreeEntry): string {
  return e.mode === '40000' ? `${e.name}/` : e.name;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = entries.slice().sort((a, b) => {
    const ka = Buffer.from(sortKey(a));
    const kb = Buffer.from(sortKey(b));
    return Buffer.compare(ka, kb);
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'), Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ commits and tags

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const info: CommitInfo = { tree: '', parents: [], author: '', committer: '', message };
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    const key = line.slice(0, sp);
    const val = line.slice(sp + 1);
    if (key === 'tree') info.tree = val;
    else if (key === 'parent') info.parents.push(val);
    else if (key === 'author') info.author = val;
    else if (key === 'committer') info.committer = val;
  }
  return info;
}

export function parseTagTarget(data: Buffer): { object: string; type: string } {
  const text = data.toString('utf8', 0, Math.min(data.length, 400));
  const object = /^object ([0-9a-f]{40})$/m.exec(text)?.[1] ?? '';
  const type = /^type (\w+)$/m.exec(text)?.[1] ?? '';
  return { object, type };
}

/** "Name <email> 1735689600 +0000" -> its parts. */
export function parseIdent(ident: string): { name: string; email: string; date: string } {
  const m = /^(.*) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(ident);
  if (!m) return { name: ident, email: '', date: new Date(0).toISOString() };
  return { name: m[1], email: m[2], date: new Date(Number(m[3]) * 1000).toISOString().replace('.000Z', 'Z') };
}

export function buildCommit(c: { tree: string; parents: string[]; author: string; committer: string; message: string }): Buffer {
  let s = `tree ${c.tree}\n`;
  for (const p of c.parents) s += `parent ${p}\n`;
  s += `author ${c.author}\ncommitter ${c.committer}\n\n${c.message}`;
  return Buffer.from(s, 'utf8');
}

// ------------------------------------------------------------------ pkt-lines

const FLUSH = Buffer.from('0000');
const DELIM = Buffer.from('0001');
export { FLUSH, DELIM };

export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'latin1');
  return Buffer.concat([head, body]);
}

export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Read pkt-lines from `buf` starting at `pos`; stops at the first flush when `untilFlush`. */
export function readPackets(buf: Buffer, pos: number, untilFlush: boolean): { packets: Packet[]; next: number } {
  const packets: Packet[] = [];
  let i = pos;
  while (i + 4 <= buf.length) {
    const lenText = buf.toString('latin1', i, i + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) throw new Error(`bad pkt-line length ${JSON.stringify(lenText)}`);
    const len = parseInt(lenText, 16);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      i += 4;
      if (untilFlush) return { packets, next: i };
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      i += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'end' });
      i += 4;
      continue;
    }
    if (len < 4 || i + len > buf.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  if (i !== buf.length && untilFlush) throw new Error('truncated pkt-line stream');
  return { packets, next: i };
}

/** Frame `data` as side-band-64k band packets (band 1 = data). */
export function sideband(band: number, data: Buffer, out: Buffer[]): void {
  const max = 65515;
  for (let i = 0; i < data.length; i += max) {
    const chunk = data.subarray(i, Math.min(data.length, i + max));
    const head = Buffer.alloc(5);
    head.write((chunk.length + 5).toString(16).padStart(4, '0'), 0, 'latin1');
    head[4] = band;
    out.push(head, chunk);
  }
}

// ------------------------------------------------------------------ packfiles

function zlibOf(obj: GitObject): Buffer {
  if (obj.z === undefined) obj.z = deflateSync(obj.data, { level: 1 });
  return obj.z;
}

/** A version-2 pack of whole (undeltified) objects, trailer included. */
export function writePack(objects: GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    let size = obj.data.length;
    const bytes: number[] = [];
    let byte = (TYPE_NUM[obj.type] << 4) | (size & 0x0f);
    size = Math.floor(size / 16);
    while (size > 0) {
      bytes.push(byte | 0x80);
      byte = size & 0x7f;
      size = Math.floor(size / 128);
    }
    bytes.push(byte);
    parts.push(Buffer.from(bytes), zlibOf(obj));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let i = 0;
  const varint = (): number => {
    let result = 0;
    let shift = 0;
    for (;;) {
      const b = delta[i++];
      result += (b & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(b & 0x80)) return result;
    }
  };
  const baseSize = varint();
  if (baseSize !== base.length) throw new Error('delta base size mismatch');
  const resultSize = varint();
  const out = Buffer.alloc(resultSize);
  let o = 0;
  while (i < delta.length) {
    const op = delta[i++];
    if (op & 0x80) {
      let off = 0;
      let len = 0;
      if (op & 0x01) off |= delta[i++];
      if (op & 0x02) off |= delta[i++] << 8;
      if (op & 0x04) off |= delta[i++] << 16;
      if (op & 0x08) off += delta[i++] * 0x1000000;
      if (op & 0x10) len |= delta[i++];
      if (op & 0x20) len |= delta[i++] << 8;
      if (op & 0x40) len |= delta[i++] << 16;
      if (len === 0) len = 0x10000;
      base.copy(out, o, off, off + len);
      o += len;
    } else if (op > 0) {
      delta.copy(out, o, i, i + op);
      o += op;
      i += op;
    } else {
      throw new Error('bad delta opcode');
    }
  }
  if (o !== resultSize) throw new Error('delta result size mismatch');
  return out;
}

function inflateAt(buf: Buffer, pos: number, expected: number): { data: Buffer; next: number } {
  const res = inflateSync(buf.subarray(pos), { info: true, finishFlush: zc.Z_SYNC_FLUSH }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  if (res.buffer.length !== expected) throw new Error('inflated size mismatch');
  return { data: res.buffer, next: pos + res.engine.bytesWritten };
}

/**
 * Parse a pack into objects keyed by oid. `lookup` resolves REF_DELTA bases that live outside the pack
 * (a thin pack). Throws on any structural error.
 */
export function readPack(pack: Buffer, lookup: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  if (pack.length === 0) return out;
  if (pack.length < 32 || pack.toString('latin1', 0, 4) !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const end = pack.length - 20;
  const sum = createHash('sha1').update(pack.subarray(0, end)).digest();
  if (!sum.equals(pack.subarray(end))) throw new Error('pack checksum mismatch');
  const byOffset = new Map<number, GitObject>();
  const pending: Array<{ offset: number; baseOffset?: number; baseOid?: string; delta: Buffer }> = [];
  let pos = 12;
  for (let n = 0; n < count; n += 1) {
    const start = pos;
    let c = pack[pos++];
    const typeNum = (c >> 4) & 7;
    let size = c & 0x0f;
    let shift = 4;
    while (c & 0x80) {
      c = pack[pos++];
      size += (c & 0x7f) * 2 ** shift;
      shift += 7;
    }
    if (typeNum === 6) {
      c = pack[pos++];
      let off = c & 0x7f;
      while (c & 0x80) {
        c = pack[pos++];
        off = (off + 1) * 128 + (c & 0x7f);
      }
      const inflated = inflateAt(pack, pos, size);
      pos = inflated.next;
      pending.push({ offset: start, baseOffset: start - off, delta: inflated.data });
    } else if (typeNum === 7) {
      const baseOid = pack.toString('hex', pos, pos + 20);
      pos += 20;
      const inflated = inflateAt(pack, pos, size);
      pos = inflated.next;
      pending.push({ offset: start, baseOid, delta: inflated.data });
    } else {
      const type = NUM_TYPE[typeNum];
      if (type === undefined) throw new Error(`bad pack object type ${typeNum}`);
      const inflated = inflateAt(pack, pos, size);
      pos = inflated.next;
      const obj: GitObject = { type, data: inflated.data };
      byOffset.set(start, obj);
      out.set(hashObject(type, obj.data), obj);
    }
    if (pos > end) throw new Error('pack entries overrun the trailer');
  }
  // Resolve deltas until no progress (bases may themselves be deltas).
  let progress = true;
  while (pending.length > 0 && progress) {
    progress = false;
    for (let k = pending.length - 1; k >= 0; k -= 1) {
      const p = pending[k];
      const base = p.baseOffset !== undefined ? byOffset.get(p.baseOffset) : out.get(p.baseOid as string) ?? lookup(p.baseOid as string);
      if (base === undefined) continue;
      const obj: GitObject = { type: base.type, data: applyDelta(base.data, p.delta) };
      byOffset.set(p.offset, obj);
      out.set(hashObject(obj.type, obj.data), obj);
      pending.splice(k, 1);
      progress = true;
    }
  }
  if (pending.length > 0) throw new Error('unresolved delta base');
  return out;
}
