import { buildApp } from '../http/router.js';
import type { RouteDefinition } from '../http/router.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import type { RelationalStore } from '../platform/index.js';
import { serviceRoutes } from '../routes/index.js';
import { deploymentKey, sealedSecrets } from '../secrets/store.js';
import { storeLocks } from '../platform/store_lock.js';
import type { SecretStore } from '../platform/secrets.js';
import { freshStore, seedClock } from './identity.js';
import { providerIdentity, stubDirectory } from './oauth.js';

export const MOUNT = '/api/v1';
export const ADMIN_TOKEN = 'af_admin_deployment_secret';
export const EXTERNAL_URL = 'https://parley.test';
/** Thirty-two bytes, so the suites exercise the real sealing. */
export const SECRET_KEY = 'c2VjcmV0LWtleS1vZi10aGlydHktdHdvLWJ5dGVzISE=';

export interface Service {
  app: ReturnType<typeof buildApp>;
  store: RelationalStore;
  secrets: SecretStore;
  dispose: () => void;
}

/**
 * The whole service over a migrated store: the real route composition, the
 * real authentication, a provider that answers from a table. A suite that
 * tests one route still reaches it the way a client does.
 */
export async function identityService(extra: RouteDefinition[] = []): Promise<Service> {
  const seeded = await freshStore();
  const clock = seedClock();
  const key = await deploymentKey(SECRET_KEY);
  const secrets = sealedSecrets(seeded.store, key as CryptoKey);
  const app = buildApp({
    routes: [
      ...serviceRoutes({
        mountPoint: MOUNT,
        buildTime: '',
        commit: '',
        store: seeded.store,
        clock,
        metrics: inMemoryMetrics(),
        providers: stubDirectory({ codes: { 'a-code': providerIdentity() } }),
        secrets,
        externalUrl: EXTERNAL_URL,
        locks: storeLocks(seeded.store, clock),
      }),
      ...extra,
    ],
    store: seeded.store,
    clock,
    auth: { store: seeded.store, clock, adminToken: ADMIN_TOKEN, mountPoint: MOUNT },
  });

  return { app, store: seeded.store, secrets, dispose: seeded.dispose };
}

/** A request as a client makes it: the credential in the usual header. */
export function as(credential: string, init: RequestInit = {}): RequestInit {
  return {
    ...init,
    headers: {
      ...((init.headers ?? {}) as Record<string, string>),
      authorization: `Bearer ${credential}`,
    },
  };
}

export function withJson(method: string, body: unknown, credential: string): RequestInit {
  return as(credential, {
    method,
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}
