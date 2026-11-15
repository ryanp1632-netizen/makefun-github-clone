// A lean HTTP/1.1 server on node:net. The suite's clients (undici fetch) send small, well-formed
// keep-alive requests; parsing them by hand and answering with one socket write per response cuts
// most of node:http's per-request overhead, which dominates the read and write workloads.
//
// Supported: keep-alive and pipelining (requests answered in order), Content-Length and chunked
// request bodies, Expect: 100-continue, Connection: close. Anything malformed gets a 400 and the
// connection is closed.

import { createServer, type Server, type Socket } from 'node:net';

export interface Req {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: Buffer;
}

export interface Res {
  headersSent: boolean;
  writeHead(status: number, headers?: Record<string, string | number>): void;
  end(body?: Buffer | string): void;
}

const REASONS: Record<number, string> = {
  100: 'Continue', 200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content', 400: 'Bad Request',
  401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found', 405: 'Method Not Allowed', 409: 'Conflict',
  413: 'Payload Too Large', 422: 'Unprocessable Entity', 500: 'Internal Server Error',
};

const MAX_HEADER = 64 * 1024;
const MAX_BODY = 512 * 1024 * 1024;
const CRLFCRLF = Buffer.from('\r\n\r\n');

class Response implements Res {
  headersSent = false;
  status = 200;
  head: Record<string, string | number> = {};
  done = false;
  socket: Socket;
  close: boolean;
  isHead: boolean;
  constructor(socket: Socket, close: boolean, isHead: boolean) {
    this.socket = socket;
    this.close = close;
    this.isHead = isHead;
  }
  writeHead(status: number, headers: Record<string, string | number> = {}): void {
    this.status = status;
    this.head = headers;
    this.headersSent = true;
  }
  end(body?: Buffer | string): void {
    if (this.done) return;
    this.done = true;
    this.headersSent = true;
    const buf = body === undefined ? null : typeof body === 'string' ? Buffer.from(body, 'utf8') : body;
    let head = `HTTP/1.1 ${this.status} ${REASONS[this.status] ?? 'Status'}\r\n`;
    let hasLength = false;
    for (const k in this.head) {
      const lk = k.toLowerCase();
      if (lk === 'content-length') {
        hasLength = true;
        if (this.status === 204) continue;
      }
      if (lk === 'connection') continue;
      head += `${k}: ${this.head[k]}\r\n`;
    }
    if (!hasLength && this.status !== 204) head += `content-length: ${buf === null ? 0 : buf.length}\r\n`;
    head += this.close ? 'connection: close\r\n\r\n' : '\r\n';
    if (this.socket.destroyed) return;
    if (buf === null || buf.length === 0 || this.isHead || this.status === 204) this.socket.write(head);
    else if (buf.length < 16384) this.socket.write(Buffer.concat([Buffer.from(head, 'latin1'), buf]));
    else {
      this.socket.cork();
      this.socket.write(head);
      this.socket.write(buf);
      this.socket.uncork();
    }
    if (this.close) this.socket.end();
  }
}

function badRequest(socket: Socket): void {
  if (!socket.destroyed) socket.end('HTTP/1.1 400 Bad Request\r\ncontent-type: application/json\r\ncontent-length: 27\r\nconnection: close\r\n\r\n{"message":"Bad Request"}\r\n');
}

/** Decode a complete chunked body starting at `pos`; null when more bytes are needed. */
function dechunk(buf: Buffer, pos: number): { body: Buffer; next: number } | null {
  const parts: Buffer[] = [];
  let i = pos;
  for (;;) {
    const eol = buf.indexOf('\r\n', i, 'latin1');
    if (eol < 0) return null;
    const size = parseInt(buf.toString('latin1', i, eol).split(';')[0], 16);
    if (!Number.isFinite(size) || size < 0) throw new Error('bad chunk');
    i = eol + 2;
    if (size === 0) {
      // skip trailers up to the blank line
      for (;;) {
        const e = buf.indexOf('\r\n', i, 'latin1');
        if (e < 0) return null;
        if (e === i) return { body: Buffer.concat(parts), next: e + 2 };
        i = e + 2;
      }
    }
    if (i + size + 2 > buf.length) return null;
    parts.push(buf.subarray(i, i + size));
    i += size + 2;
  }
}

export function createRawServer(handler: (req: Req, res: Res) => void): Server {
  const server = createServer({ noDelay: true }, (socket) => {
    socket.setNoDelay(true);
    let buf: Buffer = Buffer.alloc(0);
    let continued = false;
    socket.on('error', () => socket.destroy());
    socket.on('data', (chunk: Buffer) => {
      buf = buf.length === 0 ? chunk : Buffer.concat([buf, chunk]);
      try {
        while (buf.length > 0) {
          const end = buf.indexOf(CRLFCRLF);
          if (end < 0) {
            if (buf.length > MAX_HEADER) {
              badRequest(socket);
              buf = Buffer.alloc(0);
            }
            return;
          }
          const headText = buf.toString('latin1', 0, end);
          const lines = headText.split('\r\n');
          const first = lines[0].split(' ');
          if (first.length < 3 || !first[2].startsWith('HTTP/1.')) {
            badRequest(socket);
            buf = Buffer.alloc(0);
            return;
          }
          const headers: Record<string, string> = {};
          for (let k = 1; k < lines.length; k += 1) {
            const c = lines[k].indexOf(':');
            if (c <= 0) continue;
            const name = lines[k].slice(0, c).trim().toLowerCase();
            const value = lines[k].slice(c + 1).trim();
            headers[name] = headers[name] === undefined ? value : `${headers[name]}, ${value}`;
          }
          const bodyStart = end + 4;
          let body: Buffer;
          let next: number;
          if ((headers['transfer-encoding'] ?? '').toLowerCase().includes('chunked')) {
            const d = dechunk(buf, bodyStart);
            if (d === null) {
              if (!continued && (headers.expect ?? '').toLowerCase() === '100-continue') {
                continued = true;
                socket.write('HTTP/1.1 100 Continue\r\n\r\n');
              }
              return;
            }
            body = d.body;
            next = d.next;
          } else {
            const len = headers['content-length'] === undefined ? 0 : Number(headers['content-length']);
            if (!Number.isInteger(len) || len < 0 || len > MAX_BODY) {
              badRequest(socket);
              buf = Buffer.alloc(0);
              return;
            }
            if (buf.length < bodyStart + len) {
              if (!continued && (headers.expect ?? '').toLowerCase() === '100-continue') {
                continued = true;
                socket.write('HTTP/1.1 100 Continue\r\n\r\n');
              }
              return;
            }
            body = buf.subarray(bodyStart, bodyStart + len);
            next = bodyStart + len;
          }
          continued = false;
          buf = next >= buf.length ? Buffer.alloc(0) : buf.subarray(next);
          const close = (headers.connection ?? '').toLowerCase() === 'close' || first[2] === 'HTTP/1.0';
          const res = new Response(socket, close, first[0] === 'HEAD');
          try {
            handler({ method: first[0], url: first[1], headers, body }, res);
          } catch (err) {
            process.stderr.write(`[forge] handler threw: ${(err as Error).stack ?? String(err)}\n`);
            if (!res.done) {
              res.writeHead(500, { 'content-type': 'application/json; charset=utf-8' });
              res.end('{"message":"Internal Server Error"}\n');
            }
          }
          if (!res.done) {
            res.writeHead(500, { 'content-type': 'application/json; charset=utf-8' });
            res.end('{"message":"no response"}\n');
          }
          if (close) return;
        }
      } catch {
        badRequest(socket);
        buf = Buffer.alloc(0);
      }
    });
  });
  server.on('error', (err) => process.stderr.write(`[forge] server error: ${String(err)}\n`));
  return server;
}
