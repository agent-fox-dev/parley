import { serve } from '@hono/node-server';
import { DEFAULT_MOUNT_POINT } from '../../config.js';
import { buildApp } from '../../http/router.js';
import { systemClock } from '../../platform/local/clock.js';
import { processConfig } from '../../platform/local/config.js';
import { inMemoryMetrics } from '../../platform/local/metrics.js';
import { sqliteStore } from '../../platform/local/store.js';
import { maxBodyBytesFrom, type Platform } from '../../platform/index.js';
import { configuredProviders } from '../../auth/oauth.js';
import { serviceRoutes } from '../../routes/index.js';
import { storeLocks } from '../../platform/store_lock.js';
import { localRepositoryStore } from '../../platform/local/repostore.js';
import { sweepBindings } from '../../store/bindings.js';
import { expireStalledClones } from '../../jobs/copy_in.js';
import { sweepStalledMerges } from '../../jobs/stall_sweep.js';
import { drainJobs } from '../../jobs/drain.js';
import { storeJobs } from '../../platform/store_jobs.js';
import { deploymentKey, sealedSecrets } from '../../secrets/store.js';
import { migrate } from '../../store/migrations/index.js';
import { eventRecorder } from '../../audit/recorder.js';
import { storeBus } from '../../platform/store_bus.js';
import { signedInUserId } from '../../../web/session.js';
import { pruneDeliveries } from '../../jobs/delivery_prune.js';

/**
 * The second target: the same service on one host, with the non-Cloudflare
 * implementation of every platform interface. It needs no Cloudflare account
 * and makes no network call to Cloudflare.
 */
export function localPlatform(env: Record<string, string | undefined>): Platform {
  const config = processConfig(env);
  const store = sqliteStore(config.get('PARLEY_DB_PATH') ?? 'parley.db');
  const clock = systemClock();
  return { store, clock, config, metrics: inMemoryMetrics(), locks: storeLocks(store, clock) };
}

export function buildLocalApp(env: Record<string, string | undefined>) {
  const platform = localPlatform(env);
  const mountPoint = platform.config.get('PARLEY_MOUNT_POINT');
  const adminToken = platform.config.get('PARLEY_ADMIN_TOKEN');
  const bus = storeBus(platform.store, platform.clock);
  const events = eventRecorder({
    store: platform.store,
    clock: platform.clock,
    metrics: platform.metrics,
    bus,
  });
  const streamLimit = Number(platform.config.get('AF_SSE_MAX_CONNECTIONS') ?? '');
  return buildApp({
    store: platform.store,
    clock: platform.clock,
    metrics: platform.metrics,
    ...(mountPoint === undefined ? {} : { mountPoint }),
    ...maxBodyBytesFrom(platform.config),
    buildTime: platform.config.get('PARLEY_BUILD_TIME') ?? '',
    commit: platform.config.get('PARLEY_COMMIT') ?? '',
    routes: serviceRoutes({
      mountPoint: mountPoint ?? DEFAULT_MOUNT_POINT,
      buildTime: platform.config.get('PARLEY_BUILD_TIME') ?? '',
      commit: platform.config.get('PARLEY_COMMIT') ?? '',
      store: platform.store,
      clock: platform.clock,
      metrics: platform.metrics,
      providers: configuredProviders(platform.config),
      locks: platform.locks,
      ...(adminToken === undefined ? {} : { adminToken }),
      repos: localRepositoryStore(platform.config.get('PARLEY_REPO_PATH') ?? 'repositories'),
      events,
      bus,
      ...(Number.isInteger(streamLimit) && streamLimit > 0 ? { streamLimit } : {}),
    }),
    events,
    auth: {
      store: platform.store,
      clock: platform.clock,
      mountPoint: mountPoint ?? DEFAULT_MOUNT_POINT,
      sessions: async (value) => signedInUserId(platform.store, platform.clock, value),
      ...(adminToken === undefined ? {} : { adminToken }),
    },
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
  const platform = localPlatform(env);
  // The same schema the Worker runs, applied before the first request.
  await migrate(platform.store);
  // At start and every hour after, as the reference asks.
  await sweepBindings(platform.store, platform.clock.now().toISOString());
  await pruneDeliveries(platform.store, platform.clock);
  const sweeping = setInterval(
    () => {
      void sweepBindings(platform.store, platform.clock.now().toISOString());
      // A delivery is of use while somebody is working out why a
      // webhook did not do what they expected, and of none a week on.
      void pruneDeliveries(platform.store, platform.clock);
    },
    60 * 60 * 1000,
  );
  sweeping.unref();
  // A copy that stopped without saying so leaves a workspace nobody can
  // archive, so it is given up on rather than left.
  const expiring = setInterval(
    () => {
      void expireStalledClones(platform.store, platform.clock);
      // A merge whose runner went away leaves a job nobody is
      // running; it is given up on rather than left saying it runs.
      void sweepStalledMerges(platform.store, platform.clock);
    },
    5 * 60 * 1000,
  );
  expiring.unref();

  // The queued work — copying repositories in — runs here, outside the
  // request that asked for it.
  const key = await deploymentKey(platform.config.get('PARLEY_SECRET_KEY'));
  if (key !== undefined) {
    const deps = {
      store: platform.store,
      repos: localRepositoryStore(platform.config.get('PARLEY_REPO_PATH') ?? 'repositories'),
      secrets: sealedSecrets(platform.store, key),
      jobs: storeJobs(platform.store, platform.clock),
      clock: platform.clock,
      locks: platform.locks,
      events: eventRecorder({
        store: platform.store,
        clock: platform.clock,
        metrics: platform.metrics,
        bus: storeBus(platform.store, platform.clock),
      }),
    };
    const draining = setInterval(() => {
      void drainJobs(deps);
    }, 5000);
    draining.unref();
  }

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
