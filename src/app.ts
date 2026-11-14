// The HTTP front door: one node:http server routing /healthz, git smart HTTP, REST v3, GraphQL and
// MCP, with bearer-token auth and GitHub's JSON reply discipline.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';
import { HttpError, dispatch, type Ctx } from './rest.ts';
import { executeGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const JSON_TYPE = 'application/json; charset=utf-8';

let requestCounter = 0;
function requestId(): string {
  requestCounter += 1;
  return `F0RG:${requestCounter.toString(16).toUpperCase().padStart(8, '0')}`;
}

function baseHeaders(): Record<string, string | number> {
  return {
    'content-type': JSON_TYPE,
    'x-github-request-id': requestId(),
    'x-github-media-type': 'github.v3; format=json',
    'x-oauth-scopes': 'repo, read:org, workflow',
    'x-accepted-oauth-scopes': 'repo',
  };
}

function sendText(res: ServerResponse, status: number, text: string, extra: Record<string, string> = {}): void {
  const headers = baseHeaders();
  headers['content-length'] = Buffer.byteLength(text);
  for (const [k, v] of Object.entries(extra)) headers[k] = v;
  res.writeHead(status, headers);
  res.end(text);
}

function sendJson(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  sendText(res, status, `${JSON.stringify(body)}\n`, extra);
}

function sendError(res: ServerResponse, status: number, message: string, extra: Record<string, unknown> = {}): void {
  sendJson(res, status, { message, documentation_url: 'https://docs.github.com/rest', status: String(status), ...extra });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (c: Buffer) => {
      size += c.length;
      if (size > 256 * 1024 * 1024) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => resolve(chunks.length === 1 ? chunks[0] : Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** 'ok', 'missing' or 'bad' for the request's credentials. */
function auth(req: IncomingMessage): 'ok' | 'missing' | 'bad' {
  const h = req.headers.authorization;
  if (h === undefined || h === '') return 'missing';
  const m = /^(?:Bearer|token)\s+(\S+)\s*$/i.exec(h);
  let token = m?.[1];
  if (token === undefined && /^Basic\s+/i.test(h)) {
    const decoded = Buffer.from(h.replace(/^Basic\s+/i, ''), 'base64').toString('utf8');
    token = decoded.slice(decoded.indexOf(':') + 1);
  }
  if (token === undefined || token === '') return 'bad';
  if (TOKEN !== '' && token !== TOKEN) return 'bad';
  return 'ok';
}

function originOf(req: IncomingMessage): string {
  return `http://${req.headers.host ?? 'localhost'}`;
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const method = req.method ?? 'GET';
  const rawUrl = req.url ?? '/';
  const q = rawUrl.indexOf('?');
  const path = q < 0 ? rawUrl : rawUrl.slice(0, q);
  const search = q < 0 ? '' : rawUrl.slice(q + 1);

  if (path === '/healthz') return sendJson(res, 200, { ok: true });

  // git smart HTTP: /{owner}/{repo}.git/...
  const git = /^\/([^/]+)\/([^/]+?)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
  if (git !== null) {
    const owner = decodeURIComponent(git[1]);
    const name = decodeURIComponent(git[2]);
    if (auth(req) === 'bad') {
      res.writeHead(401, { 'content-type': JSON_TYPE, 'www-authenticate': 'Basic realm="forge"' });
      res.end('{"message":"Bad credentials"}\n');
      return;
    }
    if (git[3] === 'info/refs' && method === 'GET') return handleInfoRefs(req, res, owner, name, new URLSearchParams(search).get('service'));
    if (method === 'POST' && git[3] === 'git-upload-pack') return handleUploadPack(req, res, owner, name, await readBody(req));
    if (method === 'POST' && git[3] === 'git-receive-pack') return handleReceivePack(res, owner, name, await readBody(req));
    return sendError(res, 405, 'Method Not Allowed');
  }

  const isApi = path === '/api/v3' || path.startsWith('/api/v3/');
  const isGraphql = path === '/api/graphql' || path === '/graphql' || path === '/api/graphql/';
  const isMcp = path === '/mcp' || path === '/mcp/';

  if (!isApi && !isGraphql && !isMcp) {
    if (method === 'GET' && path === '/') return sendJson(res, 200, { message: 'forge', api: '/api/v3', graphql: '/api/graphql', mcp: '/mcp' });
    return sendError(res, 404, 'Not Found');
  }

  const a = auth(req);
  if (a !== 'ok') {
    if (isMcp) {
      res.writeHead(401, { 'content-type': JSON_TYPE, 'www-authenticate': 'Bearer realm="forge"', 'x-github-request-id': requestId() });
      res.end(`${JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized: a bearer token is required' } })}\n`);
      return;
    }
    return sendError(res, 401, a === 'missing' ? 'Requires authentication' : 'Bad credentials');
  }

  const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
  const origin = originOf(req);

  if (isMcp) {
    if (method === 'GET') return sendError(res, 405, 'Method Not Allowed: this server does not stream on GET');
    if (method === 'DELETE') {
      res.writeHead(204, { 'x-github-request-id': requestId() });
      res.end();
      return;
    }
    if (method !== 'POST') return sendError(res, 405, 'Method Not Allowed');
    const out = handleMcp(body.toString('utf8'), origin);
    if (out.body === null) {
      res.writeHead(202, { 'x-github-request-id': requestId() });
      res.end();
      return;
    }
    return sendJson(res, out.status, out.body);
  }

  if (isGraphql) {
    if (method !== 'POST') return sendError(res, 405, 'Method Not Allowed: GraphQL takes POST');
    let parsed: unknown;
    try {
      parsed = JSON.parse(body.toString('utf8'));
    } catch {
      return sendError(res, 400, 'Problems parsing JSON');
    }
    if (Array.isArray(parsed)) return sendJson(res, 200, parsed.map((op) => executeGraphql(op as Record<string, unknown>, origin)));
    return sendJson(res, 200, executeGraphql(parsed as Record<string, unknown>, origin));
  }

  // REST v3
  let parsedBody: unknown;
  if (body.length > 0) {
    try {
      parsedBody = JSON.parse(body.toString('utf8'));
    } catch {
      return sendError(res, 400, 'Problems parsing JSON');
    }
  }
  const ctx: Ctx = {
    method: method === 'HEAD' ? 'GET' : method,
    path: path.length > 7 ? path.slice(7) : '/',
    query: new URLSearchParams(search),
    body: parsedBody,
    origin,
  };
  if (ctx.path === '') ctx.path = '/';
  try {
    const reply = dispatch(ctx);
    if (reply.status === 204) {
      const headers = baseHeaders();
      delete headers['content-type'];
      res.writeHead(204, headers);
      res.end();
      return;
    }
    if (reply.raw !== undefined) return sendText(res, reply.status, reply.raw, reply.headers);
    return sendJson(res, reply.status, reply.body, reply.headers);
  } catch (err) {
    if (err instanceof HttpError) return sendError(res, err.status, err.message, err.extra);
    process.stderr.write(`[forge] ${method} ${path}: ${(err as Error).stack ?? String(err)}\n`);
    return sendError(res, 500, 'Internal Server Error');
  }
}

export function createApp(): Server {
  const server = createServer({ keepAliveTimeout: 120_000, headersTimeout: 120_000, requestTimeout: 0 }, (req, res) => {
    handle(req, res).catch((err: unknown) => {
      process.stderr.write(`[forge] unhandled: ${(err as Error)?.stack ?? String(err)}\n`);
      if (!res.headersSent) sendError(res, 500, 'Internal Server Error');
      else res.destroy();
    });
  });
  server.on('connection', (socket) => socket.setNoDelay(true));
  server.on('clientError', (_err, socket) => {
    if (socket.writable) socket.end('HTTP/1.1 400 Bad Request\r\nconnection: close\r\n\r\n');
  });
  return server;
}
