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
import { sweepBindings } from '../../store/bindings.js';
import { migrate } from '../../store/migrations/index.js';

export interface Env {
  PARLEY_MOUNT_POINT?: string;
  PARLEY_BUILD_TIME?: string;
  PARLEY_COMMIT?: string;
  PARLEY_ADMIN_TOKEN?: string;
  DB?: D1Database;
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
        }),
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
    if (env.DB !== undefined) {
      await sweepBindings(platform.store, platform.clock.now().toISOString());
    }
  },
};
