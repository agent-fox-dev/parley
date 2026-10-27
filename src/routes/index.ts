import type { RouteDefinition } from '../http/router.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import type { Clock, Locks, MetricsSink, RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';
import type { Cipher } from '../entries/store.js';
import { adminOrgRoutes } from './admin_orgs.js';
import { adminUserRoutes } from './admin_users.js';
import { authRoutes } from './auth.js';
import { keyRoutes } from './keys.js';
import { orgRoutes } from './orgs.js';
import { boundTokenRoutes } from './bound_tokens.js';
import { sandboxRoutes } from './sandboxes.js';
import { secretRoutes } from './secrets.js';
import { workspaceRoutes } from './workspaces.js';
import { tokenPermissionRoutes } from './token_permissions.js';
import { tokenRoutes } from './tokens.js';
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
  /** Absent where the deployment configured no key to seal secrets with. */
  secrets?: SecretStore;
  secretCipher?: Cipher;
  /** The deployment's own URL, for the clone URLs it advertises. */
  externalUrl?: string;
  locks: Locks;
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
    ...tokenRoutes({ mountPoint: config.mountPoint, store: config.store, clock: config.clock }),
    ...tokenPermissionRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...orgRoutes({ mountPoint: config.mountPoint, store: config.store }),
    ...sandboxRoutes(config.mountPoint),
    ...secretRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.secretCipher === undefined ? {} : { cipher: config.secretCipher }),
    }),
    ...boundTokenRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...workspaceRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      locks: config.locks,
    }),
    ...adminUserRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...adminOrgRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
  ];
}
