// The HTTP front door: routes /healthz, git smart HTTP, REST v3, GraphQL and MCP, with bearer-token
// auth and GitHub's JSON reply discipline. Served by the raw HTTP/1.1 server in http1.ts (one socket
// write per response); every handler is synchronous because the body arrives already read.

import type { Server } from 'node:net';
import { createRawServer, type Req, type Res } from './http1.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';
import { HttpError, dispatch, type Ctx } from './rest.ts';
import { executeGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const JSON_TYPE = 'application/json; charset=utf-8';
const EMPTY = new URLSearchParams('');

let requestCounter = 0;
function requestId(): string {
  requestCounter += 1;
  return `F0RG:${requestCounter.toString(16).toUpperCase()}`;
}

function headers(extra?: Record<string, string>): Record<string, string> {
  const h: Record<string, string> = {
    'content-type': JSON_TYPE,
    'x-github-request-id': requestId(),
    'x-github-media-type': 'github.v3; format=json',
    'x-oauth-scopes': 'repo, read:org, workflow',
  };
  if (extra !== undefined) for (const k in extra) h[k] = extra[k];
  return h;
}

function sendText(res: Res, status: number, text: string, extra?: Record<string, string>): void {
  res.writeHead(status, headers(extra));
  res.end(text);
}

function sendJson(res: Res, status: number, body: unknown, extra?: Record<string, string>): void {
  sendText(res, status, `${JSON.stringify(body)}\n`, extra);
}

function sendError(res: Res, status: number, message: string, extra: Record<string, unknown> = {}): void {
  sendJson(res, status, { message, documentation_url: 'https://docs.github.com/rest', status: String(status), ...extra });
}

/** 'ok', 'missing' or 'bad' for the request's credentials. */
function auth(req: Req): 'ok' | 'missing' | 'bad' {
  const h = req.headers.authorization;
  if (h === undefined || h === '') return 'missing';
  let token: string | undefined;
  if (h.startsWith('Bearer ')) token = h.slice(7).trim();
  else {
    const m = /^(?:Bearer|token)\s+(\S+)\s*$/i.exec(h);
    token = m?.[1];
    if (token === undefined && /^Basic\s+/i.test(h)) {
      const decoded = Buffer.from(h.replace(/^Basic\s+/i, ''), 'base64').toString('utf8');
      token = decoded.slice(decoded.indexOf(':') + 1);
    }
  }
  if (token === undefined || token === '') return 'bad';
  if (TOKEN !== '' && token !== TOKEN) return 'bad';
  return 'ok';
}

const GIT_RE = /^\/([^/]+)\/([^/]+?)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/;

function handle(req: Req, res: Res): void {
  const method = req.method;
  const rawUrl = req.url;
  const q = rawUrl.indexOf('?');
  const path = q < 0 ? rawUrl : rawUrl.slice(0, q);
  const search = q < 0 ? '' : rawUrl.slice(q + 1);

  if (path === '/healthz') return sendJson(res, 200, { ok: true });

  const isApi = path.startsWith('/api/v3');
  if (!isApi && (path.endsWith('.git/info/refs') || path.endsWith('.git/git-upload-pack') || path.endsWith('.git/git-receive-pack'))) {
    const git = GIT_RE.exec(path);
    if (git !== null) {
      const owner = decodeURIComponent(git[1]);
      const name = decodeURIComponent(git[2]);
      if (auth(req) === 'bad') {
        res.writeHead(401, { 'content-type': JSON_TYPE, 'www-authenticate': 'Basic realm="forge"' });
        res.end('{"message":"Bad credentials"}\n');
        return;
      }
      if (git[3] === 'info/refs' && method === 'GET') return handleInfoRefs(req, res, owner, name, new URLSearchParams(search).get('service'));
      if (method === 'POST' && git[3] === 'git-upload-pack') return handleUploadPack(req, res, owner, name, req.body);
      if (method === 'POST' && git[3] === 'git-receive-pack') return handleReceivePack(res, owner, name, req.body);
      return sendError(res, 405, 'Method Not Allowed');
    }
  }

  const isRestApi = isApi && (path.length === 7 || path.charCodeAt(7) === 47);
  const isGraphql = path === '/api/graphql' || path === '/graphql' || path === '/api/graphql/';
  const isMcp = path === '/mcp' || path === '/mcp/';

  if (!isRestApi && !isGraphql && !isMcp) {
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

  const body = req.body;
  const origin = `http://${req.headers.host ?? 'localhost'}`;

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
    query: search === '' ? EMPTY : new URLSearchParams(search),
    body: parsedBody,
    origin,
  };
  try {
    const reply = dispatch(ctx);
    if (reply.status === 204) {
      const h = headers();
      delete h['content-type'];
      res.writeHead(204, h);
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
  return createRawServer(handle);
}
