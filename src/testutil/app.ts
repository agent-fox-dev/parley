import { buildApp } from '../http/router.js';
import type { RouteDefinition } from '../http/router.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import type { RelationalStore } from '../platform/index.js';
import { serviceRoutes } from '../routes/index.js';
import { deploymentKey, openWith, sealWith, sealedSecrets } from '../secrets/store.js';
import { storeLocks } from '../platform/store_lock.js';
import { storeJobs } from '../platform/store_jobs.js';
import { storeBus } from '../platform/store_bus.js';
import type { Locks } from '../platform/lock.js';
import type { SecretStore } from '../platform/secrets.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { JobQueue } from '../platform/jobs.js';
import type { Decider } from '../git/push.js';
import { eventRecorder } from '../audit/recorder.js';
import type { EventRecorder } from '../audit/recorder.js';
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
  /** The recorder the routes write their events to. */
  events: EventRecorder;
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
    /** A recorder of the suite's own, or 'raising' for one that fails. */
    events?: EventRecorder | 'raising';
    /** How many event streams the service will serve at once. */
    streamLimit?: number;
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
  const bus = storeBus(seeded.store, clock);
  const events: EventRecorder =
    options.events === 'raising'
      ? {
          record: () => Promise.reject(new Error('the recorder is broken')),
          refusal: () => Promise.reject(new Error('the recorder is broken')),
        }
      : (options.events ?? eventRecorder({ store: seeded.store, clock, bus }));
  const app = buildApp({
    routes: [
      ...serviceRoutes({
        mountPoint: MOUNT,
        buildTime: '',
        commit: '',
        store: seeded.store,
        clock,
        metrics: inMemoryMetrics(),
        providers: stubDirectory({
          codes: {
            'a-code': providerIdentity(),
            'b-code': providerIdentity({ username: 'bob', provider_id: '67890' }),
          },
        }),
        secrets,
        secretCipher,
        externalUrl: options.externalUrl ?? EXTERNAL_URL,
        ...(options.repos === undefined ? {} : { repos: options.repos }),
        ...(jobs === undefined ? {} : { jobs }),
        fetch: options.fetch ?? noNetwork,
        locks,
        adminToken: ADMIN_TOKEN,
        ...(options.decideRef === undefined ? {} : { decideRef: options.decideRef }),
        events,
        bus,
        ...(options.streamLimit === undefined ? {} : { streamLimit: options.streamLimit }),
      }),
      ...extra,
    ],
    store: seeded.store,
    clock,
    auth: { store: seeded.store, clock, adminToken: ADMIN_TOKEN, mountPoint: MOUNT },
    events,
  });

  return { app, store: seeded.store, secrets, locks, jobs, events, dispose: seeded.dispose };
}

/** One event the service recorded, as a suite reads it back. */
export interface RecordedEvent {
  event_type: string;
  action: string;
  workspace: string | null;
  actor_type: string;
  resource_id: string;
  metadata: Record<string, unknown>;
}

/**
 * What the service wrote down, oldest first. Suites of the routes that
 * record an event read it back from here rather than each writing the
 * same query.
 */
export async function recordedEvents(
  store: RelationalStore,
  type?: string,
): Promise<RecordedEvent[]> {
  const rows = await store.all<{
    event_type: string;
    action: string;
    workspace: string | null;
    actor_type: string;
    resource_id: string;
    metadata: string;
    timestamp: string;
  }>(
    `SELECT event_type, action, workspace, actor_type, resource_id, metadata, timestamp
       FROM hub_events ${type === undefined ? '' : 'WHERE event_type = ?'}
      ORDER BY timestamp, id`,
    type === undefined ? [] : [type],
  );
  return rows.map((row) => ({
    event_type: row.event_type,
    action: row.action,
    workspace: row.workspace,
    actor_type: row.actor_type,
    resource_id: row.resource_id,
    metadata: JSON.parse(row.metadata) as Record<string, unknown>,
  }));
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
