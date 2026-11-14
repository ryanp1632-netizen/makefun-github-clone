// Entry point: start the forge on an ephemeral port, register it with the trial's env, and stay up
// until the env says the suite is done.
//
//   1. listen(0) — the kernel picks a free port (never 8080, which the env owns);
//   2. POST {"port": N} to $MAKEFUN_ENV_URL/register (retried while the env starts; 409 = already in);
//   3. poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
// Without MAKEFUN_ENV_URL the forge just serves (on FORGE_PORT or an ephemeral port) for local use.

import { createApp } from './src/app.ts';

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

async function main(): Promise<void> {
  const server = createApp();
  const wanted = ENV_URL === '' ? Number(process.env.FORGE_PORT ?? 0) : 0;
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(wanted, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('no TCP port bound');
  process.stdout.write(`[forge] listening on port ${address.port}\n`);
  if (ENV_URL === '') return; // local mode: serve until killed
  await register(address.port);
  process.stdout.write('[forge] registered; waiting for done\n');
  await waitForDone();
  server.close();
  process.stdout.write('[forge] done\n');
  process.exit(0);
}

main().catch((err: unknown) => {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
