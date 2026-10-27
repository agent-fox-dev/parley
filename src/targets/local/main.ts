import { serve } from '@hono/node-server';
import { buildApp } from '../../http/router.js';
import { systemClock } from '../../platform/local/clock.js';
import { processConfig } from '../../platform/local/config.js';
import { inMemoryMetrics } from '../../platform/local/metrics.js';
import { sqliteStore } from '../../platform/local/store.js';
import type { Platform } from '../../platform/index.js';

/**
 * The second target: the same service on one host, with the non-Cloudflare
 * implementation of every platform interface. It needs no Cloudflare account
 * and makes no network call to Cloudflare.
 */
export function localPlatform(env: Record<string, string | undefined>): Platform {
  const config = processConfig(env);
  return {
    store: sqliteStore(config.get('PARLEY_DB_PATH') ?? 'parley.db'),
    clock: systemClock(),
    config,
    metrics: inMemoryMetrics(),
  };
}

export function buildLocalApp(env: Record<string, string | undefined>) {
  const platform = localPlatform(env);
  const mountPoint = platform.config.get('PARLEY_MOUNT_POINT');
  return buildApp({
    store: platform.store,
    clock: platform.clock,
    metrics: platform.metrics,
    ...(mountPoint === undefined ? {} : { mountPoint }),
    buildTime: platform.config.get('PARLEY_BUILD_TIME') ?? '',
    commit: platform.config.get('PARLEY_COMMIT') ?? '',
  });
}

export function startLocalTarget(env: Record<string, string | undefined> = process.env): {
  port: number;
  stop: () => Promise<void>;
} {
  const requested = Number(env['PARLEY_PORT'] ?? '8787');
  const server = serve({ fetch: buildLocalApp(env).fetch, port: requested });
  const address = server.address();
  const port = typeof address === 'object' && address !== null ? address.port : requested;

  return {
    port,
    stop: () =>
      new Promise<void>((resolve, reject) => {
        server.close((err) => (err === undefined || err === null ? resolve() : reject(err)));
      }),
  };
}

// Started as a program rather than imported as a module.
if (
  process.argv[1] !== undefined &&
  import.meta.url.endsWith(process.argv[1].split('/').pop() ?? '\u0000')
) {
  const { port } = startLocalTarget();
  console.log(`parley listening on http://127.0.0.1:${port}`);
}
