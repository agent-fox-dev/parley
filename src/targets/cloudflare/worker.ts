import { buildApp } from '../../http/router.js';
import { envConfig } from '../../platform/cloudflare/config.js';
import { d1Store } from '../../platform/cloudflare/store.js';
import { systemClock } from '../../platform/local/clock.js';
import { inMemoryMetrics } from '../../platform/local/metrics.js';
import type { Platform } from '../../platform/index.js';

export interface Env {
  PARLEY_MOUNT_POINT?: string;
  PARLEY_BUILD_TIME?: string;
  PARLEY_COMMIT?: string;
  DB?: D1Database;
}

/** Assembles the platform a Worker is deployed with. */
export function cloudflarePlatform(env: Env): Platform {
  const config = envConfig(env as unknown as Record<string, unknown>);
  return {
    store: env.DB === undefined ? { ping: async () => Promise.resolve() } : d1Store(env.DB),
    clock: systemClock(),
    config,
    metrics: inMemoryMetrics(),
  };
}

// Built once per isolate: the metrics sink must outlive a single request.
let app: ReturnType<typeof buildApp> | undefined;

export default {
  fetch(request: Request, env: Env, ctx: ExecutionContext): Response | Promise<Response> {
    if (app === undefined) {
      const platform = cloudflarePlatform(env);
      app = buildApp({
        store: platform.store,
        clock: platform.clock,
        metrics: platform.metrics,
        ...(platform.config.get('PARLEY_MOUNT_POINT') === undefined
          ? {}
          : { mountPoint: platform.config.get('PARLEY_MOUNT_POINT') as string }),
        buildTime: platform.config.get('PARLEY_BUILD_TIME') ?? '',
        commit: platform.config.get('PARLEY_COMMIT') ?? '',
      });
    }
    return app.fetch(request, env, ctx);
  },
};
