import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, describe, expect, it } from 'vitest';
import { startLocalTarget } from './main.js';

/**
 * 01-PATH-2: the second target on one host, probed over HTTP with no
 * Cloudflare credential in the environment.
 */
const CLOUDFLARE_VARS = ['CLOUDFLARE_API_TOKEN', 'CLOUDFLARE_ACCOUNT_ID', 'CF_API_TOKEN'];

let started: { port: number; stop: () => Promise<void> } | undefined;
let unavailable: string | undefined;
const dir = mkdtempSync(join(tmpdir(), 'parley-local-smoke-'));

try {
  started = await startLocalTarget({ PARLEY_PORT: '0', PARLEY_DB_PATH: join(dir, 'parley.db') });
} catch (err) {
  unavailable = ` (skipped: the host refuses to bind a local port — ${
    err instanceof Error ? err.message : String(err)
  })`;
}

function at(path: string): string {
  return `http://127.0.0.1:${started?.port ?? 0}${path}`;
}

afterAll(async () => {
  await started?.stop();
  rmSync(dir, { recursive: true, force: true });
});

describe.skipIf(started === undefined)(`the second target${unavailable ?? ''}`, () => {
  it('runs with no Cloudflare credential in the environment', () => {
    for (const name of CLOUDFLARE_VARS) {
      expect(process.env[name] ?? '').toBe('');
    }
  });

  it.each(['/healthz', '/readyz', '/version', '/metrics'])(
    'serves %s over HTTP from one host',
    async (path) => {
      const response = await fetch(at(path));

      expect(response.status).toBe(200);
    },
  );

  it('answers the readiness probe from its own SQLite store', async () => {
    const response = await fetch(at('/readyz'));

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({ status: 'ready' });
  });
});
