import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import { SERVER_ROUTE_PATHS } from '../routes/server.js';
import { envConfig } from './cloudflare/config.js';
import { d1Store } from './cloudflare/store.js';
import { systemClock } from './local/clock.js';
import { storeLocks } from './store_lock.js';
import { inMemoryMetrics } from './local/metrics.js';
import { processConfig } from './local/config.js';
import { sqliteStore } from './local/store.js';
import type { Platform } from './index.js';

/**
 * The same service, wired to each platform's implementations in turn, must
 * answer the four server routes alike. Values that are genuinely per
 * deployment — the build information and whatever the metrics happen to have
 * measured — are allowed to differ; nothing else is.
 */

interface Wiring {
  name: string;
  platform: Platform;
  /** Puts the wiring's store into the state a readiness probe should fail on. */
  breakStore: () => void;
}

const dirs: string[] = [];

function freshDir(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-parity-'));
  dirs.push(dir);
  return dir;
}

function cloudflareWiring(): Wiring {
  let answering = true;
  const db = {
    prepare: () => ({
      first: async () => {
        if (!answering) {
          throw new Error('D1_ERROR: the database is unreachable');
        }
        return { one: 1 };
      },
    }),
  };
  return {
    name: 'the Cloudflare wiring',
    platform: {
      store: d1Store(db as never),
      clock: systemClock(),
      config: envConfig({ PARLEY_MOUNT_POINT: '/api/v1' }),
      metrics: inMemoryMetrics(),
      locks: storeLocks(d1Store(db as never), systemClock()),
    },
    breakStore: () => {
      answering = false;
    },
  };
}

function localWiring(): Wiring {
  const dir = freshDir();
  return {
    name: 'the non-Cloudflare wiring',
    platform: {
      store: sqliteStore(join(dir, 'parley.db')),
      clock: systemClock(),
      config: processConfig({ PARLEY_MOUNT_POINT: '/api/v1' }),
      metrics: inMemoryMetrics(),
      locks: storeLocks(sqliteStore(join(dir, 'parley.db')), systemClock()),
    },
    breakStore: () => {
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

function appFor(wiring: Wiring) {
  const { platform } = wiring;
  return buildApp({
    store: platform.store,
    clock: platform.clock,
    metrics: platform.metrics,
    ...(platform.config.get('PARLEY_MOUNT_POINT') === undefined
      ? {}
      : { mountPoint: platform.config.get('PARLEY_MOUNT_POINT') as string }),
  });
}

async function metricNames(wiring: Wiring): Promise<Array<string | undefined>> {
  const text = await (await appFor(wiring).request('/metrics')).text();
  return text
    .split('\n')
    .filter((line) => line.startsWith('# TYPE '))
    .map((line) => line.split(' ')[2])
    .toSorted();
}

async function probe(wiring: Wiring) {
  const app = appFor(wiring);
  const before: Record<string, { status: number; keys: string[] }> = {};

  const answers = await Promise.all(
    SERVER_ROUTE_PATHS.map(async (path) => {
      const response = await app.request(path);
      return { path, status: response.status, text: await response.text() };
    }),
  );
  for (const answer of answers) {
    let keys: string[] = [];
    try {
      const parsed: unknown = JSON.parse(answer.text);
      keys = typeof parsed === 'object' && parsed !== null ? Object.keys(parsed).toSorted() : [];
    } catch {
      keys = ['<not json>'];
    }
    before[answer.path] = { status: answer.status, keys };
  }

  wiring.breakStore();
  const afterBreak = await app.request('/readyz');
  const afterBody = (await afterBreak.json()) as { error?: { code?: number } };

  return { before, readyzAfterBreak: { status: afterBreak.status, code: afterBody.error?.code } };
}

describe('the two platform wirings', () => {
  afterEach(() => {
    for (const dir of dirs.splice(0)) {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('answer the four server routes with the same status and the same body keys', async () => {
    const cloudflare = await probe(cloudflareWiring());
    const local = await probe(localWiring());

    expect(local.before).toEqual(cloudflare.before);
  });

  it('both answer the readiness probe 503 in the error envelope once the store is unreachable', async () => {
    const cloudflare = await probe(cloudflareWiring());
    const local = await probe(localWiring());

    expect(cloudflare.readyzAfterBreak).toEqual({ status: 503, code: 503 });
    expect(local.readyzAfterBreak).toEqual({ status: 503, code: 503 });
  });

  it('serve the metrics exposition with the same metric names under both wirings', async () => {
    expect(await metricNames(localWiring())).toEqual(await metricNames(cloudflareWiring()));
  });
});
