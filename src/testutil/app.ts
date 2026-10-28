import { buildApp } from '../http/router.js';
import type { RouteDefinition } from '../http/router.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import type { RelationalStore } from '../platform/index.js';
import { serviceRoutes } from '../routes/index.js';
import { deploymentKey, openWith, sealWith, sealedSecrets } from '../secrets/store.js';
import { storeLocks } from '../platform/store_lock.js';
import { storeJobs } from '../platform/store_jobs.js';
import type { Locks } from '../platform/lock.js';
import type { SecretStore } from '../platform/secrets.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { JobQueue } from '../platform/jobs.js';
import type { Decider } from '../git/push.js';
import { freshStore, seedClock, tickingClock } from './identity.js';
import { providerIdentity, stubDirectory } from './oauth.js';

/**
 * What a suite that passed no fetch gets. Nothing in the fast gate
 * may reach the network: a suite that does is not hermetic, and its
 * result depends on whatever a proxy or a forge is doing at the time
 * — which is how a passing suite becomes an intermittent one.
 */
const noNetwork: typeof fetch = () => {
  throw new TypeError('this suite reached the network; give identityService a fetch of its own');
};

export const MOUNT = '/api/v1';
export const ADMIN_TOKEN = 'af_admin_deployment_secret';
export const EXTERNAL_URL = 'https://parley.test';
/** Thirty-two bytes, so the suites exercise the real sealing. */
export const SECRET_KEY = 'c2VjcmV0LWtleS1vZi10aGlydHktdHdvLWJ5dGVzISE=';

export interface Service {
  app: ReturnType<typeof buildApp>;
  store: RelationalStore;
  secrets: SecretStore;
  /** The same locks the routes hold, so a suite can hold one itself. */
  locks: Locks;
  /** The queue the routes enqueue onto, where the suite asked for one. */
  jobs?: JobQueue;
  dispose: () => void;
}

/**
 * The whole service over a migrated store: the real route composition, the
 * real authentication, a provider that answers from a table. A suite that
 * tests one route still reaches it the way a client does.
 */
export async function identityService(
  extra: RouteDefinition[] = [],
  options: {
    repos?: RepositoryStore;
    /** A queue of the suite's own, or 'store' for one over this store. */
    jobs?: JobQueue | 'store';
    fetch?: typeof fetch;
    /** Overridden where a suite serves the app at an address of its own. */
    externalUrl?: string;
    /** What the service makes of each ref of a git push. */
    decideRef?: Decider;
    /** 'ticking' for a clock that moves, where a suite needs order. */
    clock?: 'fixed' | 'ticking';
  } = {},
): Promise<Service> {
  const seeded = await freshStore();
  const clock = options.clock === 'ticking' ? tickingClock() : seedClock();
  const key = await deploymentKey(SECRET_KEY);
  const secrets = sealedSecrets(seeded.store, key as CryptoKey);
  const secretCipher = { seal: sealWith(key as CryptoKey), open: openWith(key as CryptoKey) };
  // The same locks the routes hold, so a suite can hold one itself.
  const locks = storeLocks(seeded.store, clock);
  const jobs = options.jobs === 'store' ? storeJobs(seeded.store, clock) : options.jobs;
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
        secretCipher,
        externalUrl: options.externalUrl ?? EXTERNAL_URL,
        ...(options.repos === undefined ? {} : { repos: options.repos }),
        ...(jobs === undefined ? {} : { jobs }),
        fetch: options.fetch ?? noNetwork,
        locks,
        adminToken: ADMIN_TOKEN,
        ...(options.decideRef === undefined ? {} : { decideRef: options.decideRef }),
      }),
      ...extra,
    ],
    store: seeded.store,
    clock,
    auth: { store: seeded.store, clock, adminToken: ADMIN_TOKEN, mountPoint: MOUNT },
  });

  return { app, store: seeded.store, secrets, locks, jobs, dispose: seeded.dispose };
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
