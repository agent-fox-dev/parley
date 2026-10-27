import { serve } from '@hono/node-server';
import { buildApp } from '../../http/router.js';
import { systemClock } from '../../platform/local/clock.js';
import { processConfig } from '../../platform/local/config.js';
import { inMemoryMetrics } from '../../platform/local/metrics.js';
import { sqliteStore } from '../../platform/local/store.js';
import { maxBodyBytesFrom, type Platform } from '../../platform/index.js';

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
    ...maxBodyBytesFrom(platform.config),
    buildTime: platform.config.get('PARLEY_BUILD_TIME') ?? '',
    commit: platform.config.get('PARLEY_COMMIT') ?? '',
  });
}

/**
 * Starts the second target and resolves once it is listening. A host that
 * refuses to bind rejects here rather than leaving a server that answers
 * nothing.
 */
export async function startLocalTarget(
  env: Record<string, string | undefined> = process.env,
): Promise<{ port: number; stop: () => Promise<void> }> {
  const requested = Number(env['PARLEY_PORT'] ?? '8787');

  return new Promise((resolve, reject) => {
    const server = serve({ fetch: buildLocalApp(env).fetch, port: requested }, (info) => {
      resolve({
        port: info.port,
        stop: () =>
          new Promise<void>((done, failed) => {
            server.close((err) => (err === undefined || err === null ? done() : failed(err)));
          }),
      });
    });
    server.on('error', reject);
  });
}

// Started as a program rather than imported as a module.
if (
  process.argv[1]?.endsWith('parley.mjs') === true ||
  process.argv[1]?.endsWith('main.ts') === true
) {
  const { port } = await startLocalTarget();
  console.log(`parley listening on http://127.0.0.1:${port}`);
}
