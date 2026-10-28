import { eventRecorder } from '../../audit/recorder.js';
import { storeBus } from '../../platform/store_bus.js';
import { DEFAULT_MOUNT_POINT } from '../../config.js';
import { buildApp } from '../../http/router.js';
import { envConfig } from '../../platform/cloudflare/config.js';
import { d1Store } from '../../platform/cloudflare/store.js';
import { noStore } from '../../platform/store.js';
import { systemClock } from '../../platform/local/clock.js';
import { inMemoryMetrics } from '../../platform/local/metrics.js';
import { maxBodyBytesFrom, type Platform } from '../../platform/index.js';
import { configuredProviders } from '../../auth/oauth.js';
import { serviceRoutes } from '../../routes/index.js';
import { storeLocks } from '../../platform/store_lock.js';
import {
  type ArtifactsBinding,
  artifactsRepositoryStore,
} from '../../platform/cloudflare/artifacts.js';
import { sweepBindings } from '../../store/bindings.js';
import { expireStalledClones } from '../../jobs/copy_in.js';
import { sweepStalledMerges } from '../../jobs/stall_sweep.js';
import { drainJobs } from '../../jobs/drain.js';
import { storeJobs } from '../../platform/store_jobs.js';
import { deploymentKey, sealedSecrets } from '../../secrets/store.js';
import { migrate } from '../../store/migrations/index.js';

export interface Env {
  PARLEY_MOUNT_POINT?: string;
  PARLEY_BUILD_TIME?: string;
  PARLEY_COMMIT?: string;
  PARLEY_ADMIN_TOKEN?: string;
  DB?: D1Database;
  ARTIFACTS?: unknown;
}

/** Assembles the platform a Worker is deployed with. */
export function cloudflarePlatform(env: Env): Platform {
  const config = envConfig(env as unknown as Record<string, unknown>);
  const store = env.DB === undefined ? noStore() : d1Store(env.DB);
  const clock = systemClock();
  return { store, clock, config, metrics: inMemoryMetrics(), locks: storeLocks(store, clock) };
}

// Built once per isolate: the metrics sink must outlive a single request.
let app: ReturnType<typeof buildApp> | undefined;
let schema: Promise<void> | undefined;

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    if (app === undefined) {
      const platform = cloudflarePlatform(env);
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
      app = buildApp({
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
          ...(env.ARTIFACTS === undefined
            ? {}
            : { repos: artifactsRepositoryStore(env.ARTIFACTS as ArtifactsBinding) }),
          events,
          bus,
          ...(Number.isInteger(streamLimit) && streamLimit > 0 ? { streamLimit } : {}),
        }),
        events,
        auth: {
          store: platform.store,
          clock: platform.clock,
          mountPoint: mountPoint ?? DEFAULT_MOUNT_POINT,
          ...(adminToken === undefined ? {} : { adminToken }),
        },
      });
      // The same schema both targets run, applied once per isolate.
      schema = env.DB === undefined ? Promise.resolve() : migrate(platform.store);
    }
    await schema;
    return app.fetch(request, env, ctx);
  },

  /**
   * The hourly sweep, run by a cron trigger. It retires the bindings that
   * a workspace deletion or a token revocation left behind.
   */
  async scheduled(_event: ScheduledController, env: Env): Promise<void> {
    const platform = cloudflarePlatform(env);
    if (env.DB === undefined) {
      return;
    }
    await sweepBindings(platform.store, platform.clock.now().toISOString());
    await expireStalledClones(platform.store, platform.clock);
    await sweepStalledMerges(platform.store, platform.clock);

    // The queued work runs here: a copy continues a batch at a time
    // until it is finished.
    const key = await deploymentKey(platform.config.get('PARLEY_SECRET_KEY'));
    if (key !== undefined && env.ARTIFACTS !== undefined) {
      await drainJobs({
        store: platform.store,
        repos: artifactsRepositoryStore(env.ARTIFACTS as ArtifactsBinding),
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
      });
    }
  },
};
