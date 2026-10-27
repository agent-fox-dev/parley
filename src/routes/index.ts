import type { RouteDefinition } from '../http/router.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import type { Clock, MetricsSink, RelationalStore } from '../platform/index.js';
import { authRoutes } from './auth.js';
import { keyRoutes } from './keys.js';
import { userRoutes } from './user.js';
import { serverRoutes } from './server.js';

export interface ServiceRouteConfig {
  mountPoint: string;
  buildTime: string;
  commit: string;
  store: RelationalStore;
  clock: Clock;
  metrics: MetricsSink;
  providers: ProviderDirectory;
}

/**
 * Every route the service serves, in one place, so that a target wires the
 * whole surface rather than remembering to add each new group to its own
 * list.
 */
export function serviceRoutes(config: ServiceRouteConfig): RouteDefinition[] {
  return [
    ...serverRoutes({
      mountPoint: config.mountPoint,
      buildTime: config.buildTime,
      commit: config.commit,
      store: config.store,
      metrics: config.metrics,
    }),
    ...authRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      providers: config.providers,
    }),
    ...userRoutes({ mountPoint: config.mountPoint, store: config.store, clock: config.clock }),
    ...keyRoutes({ mountPoint: config.mountPoint, store: config.store, clock: config.clock }),
  ];
}
