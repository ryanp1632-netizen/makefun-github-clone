// Entry point: start the forge on an ephemeral port, register it with the trial's env, and stay up
// until the env says the suite is done.
//
//   1. listen(0) — the kernel picks a free port (never 8080, which the env owns);
//   2. POST {"port": N} to $MAKEFUN_ENV_URL/register (retried while the env starts; 409 = already in);
//   3. poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
// Without MAKEFUN_ENV_URL the forge just serves (on FORGE_PORT or an ephemeral port) for local use.
//
// The server itself is the native build of the same forge (native/*.go, prebuilt into bin/ for
// linux-amd64, since the grader image has only Node). If that binary is missing, cannot start on this
// platform, or does not answer /healthz, the forge falls back to the Node implementation in src/.
// FORGE_NATIVE=0 forces the Node implementation.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createApp } from './src/app.ts';

function nativeBinary(): string | null {
  if (process.env.FORGE_NATIVE === '0') return null;
  const arch = process.arch === 'x64' ? 'amd64' : process.arch;
  const name = process.platform === 'win32' ? `forge-windows-${arch}.exe` : `forge-${process.platform}-${arch}`;
  const file = join(dirname(fileURLToPath(import.meta.url)), 'bin', name);
  return existsSync(file) ? file : null;
}

async function healthy(port: number): Promise<boolean> {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(2_000) });
      const body = (await res.json()) as { ok?: unknown };
      if (res.ok && body.ok === true) return true;
    } catch {
      // not answering yet
    }
    await sleep(100);
  }
  return false;
}

/** Start the native server; resolves to its port, or null when the Node fallback should serve. */
function startNative(port: number): Promise<{ port: number; child: ChildProcess } | null> {
  const file = nativeBinary();
  if (file === null) return Promise.resolve(null);
  try {
    chmodSync(file, 0o755);
  } catch {
    // read-only checkout: the mode from git is used as is
  }
  return new Promise((resolve) => {
    let settled = false;
    let child: ChildProcess;
    const finish = (value: { port: number; child: ChildProcess } | null): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (value === null) {
        try {
          child?.kill('SIGKILL');
        } catch {
          // already gone
        }
      }
      resolve(value);
    };
    const timer = setTimeout(() => finish(null), 15_000);
    try {
      child = spawn(file, [], { env: { ...process.env, FORGE_PORT: String(port) }, stdio: ['ignore', 'pipe', 'inherit'] });
    } catch {
      finish(null);
      return;
    }
    let head = '';
    child.stdout?.on('data', (chunk: Buffer) => {
      if (settled) {
        process.stdout.write(chunk);
        return;
      }
      head += chunk.toString('utf8');
      const m = /listening on port (\d+)/.exec(head);
      if (m === null) return;
      const p = Number(m[1]);
      healthy(p).then((ok) => finish(ok ? { port: p, child } : null), () => finish(null));
    });
    child.once('error', () => finish(null));
    child.once('exit', (code, signal) => {
      if (!settled) {
        finish(null);
        return;
      }
      if (exiting || native === null || native.child !== child) return;
      process.stderr.write(`[forge] native server exited (code ${code}, signal ${signal})\n`);
      process.exit(1);
    });
  });
}

let exiting = false;
let native: { port: number; child: ChildProcess } | null = null;

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
const backoff = (attempt: number): number => Math.min(2000, 250 * 2 ** Math.min(attempt, 8));

process.on('uncaughtException', (err) => {
  process.stderr.write(`[forge] uncaught: ${err.stack ?? String(err)}\n`);
});
process.on('unhandledRejection', (err) => {
  process.stderr.write(`[forge] unhandled rejection: ${String(err)}\n`);
});

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(30_000),
      });
      await res.arrayBuffer().catch(() => undefined);
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered')) throw err;
    }
    await sleep(backoff(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(30_000) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // not answering yet: poll again
    }
    await sleep(backoff(attempt));
  }
}

async function listenNode(wanted: number): Promise<{ port: number; close: () => void }> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(wanted, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('no TCP port bound');
  process.stdout.write(`[forge] listening on port ${address.port}\n`);
  return { port: address.port, close: () => server.close() };
}

async function main(): Promise<void> {
  const wanted = ENV_URL === '' ? Number(process.env.FORGE_PORT ?? 0) : 0;
  native = await startNative(wanted);
  let port: number;
  let close: () => void;
  if (native !== null) {
    const child = native.child;
    port = native.port;
    close = () => {
      exiting = true;
      child.kill('SIGTERM');
    };
    process.stdout.write(`[forge] native server on port ${port}\n`);
    process.once('exit', () => {
      exiting = true;
      try {
        child.kill('SIGTERM');
      } catch {
        // already gone
      }
    });
    for (const sig of ['SIGTERM', 'SIGINT'] as const) {
      process.on(sig, () => {
        close();
        process.exit(0);
      });
    }
  } else {
    process.stdout.write('[forge] native server unavailable; serving from Node\n');
    ({ port, close } = await listenNode(wanted));
  }
  if (ENV_URL === '') return; // local mode: serve until killed
  await register(port);
  process.stdout.write('[forge] registered; waiting for done\n');
  await waitForDone();
  close();
  process.stdout.write('[forge] done\n');
  process.exit(0);
}

main().catch((err: unknown) => {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
