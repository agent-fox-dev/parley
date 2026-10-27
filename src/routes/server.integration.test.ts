import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import { sqliteStore } from '../platform/local/store.js';

/**
 * The readiness probe against a real store, reached through the platform
 * interface, with the store made genuinely unreachable in between.
 */
describe('the readiness probe against a real store', () => {
  const dirs: string[] = [];

  afterEach(() => {
    for (const dir of dirs.splice(0)) {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  function freshStoreDir(): string {
    const dir = mkdtempSync(join(tmpdir(), 'parley-readyz-'));
    dirs.push(dir);
    return dir;
  }

  it('answers ready, then 503 once the store is unreachable, then ready again', async () => {
    const dir = freshStoreDir();
    const app = buildApp({ store: sqliteStore(join(dir, 'parley.db')) });

    const first = await app.request('/readyz');
    expect(first.status).toBe(200);
    await expect(first.json()).resolves.toEqual({ status: 'ready' });

    rmSync(dir, { recursive: true, force: true });

    const second = await app.request('/readyz');
    const body = (await second.json()) as { error: { code: number; message: string } };
    expect(second.status).toBe(503);
    expect(body.error.code).toBe(503);
    expect(body.error.message.length).toBeGreaterThan(0);

    mkdtempSync(join(tmpdir(), 'parley-readyz-'));
    const recovered = buildApp({ store: sqliteStore(join(freshStoreDir(), 'parley.db')) });
    const third = await recovered.request('/readyz');
    expect(third.status).toBe(200);
    await expect(third.json()).resolves.toEqual({ status: 'ready' });
  });
});
