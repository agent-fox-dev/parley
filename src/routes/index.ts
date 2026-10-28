import type { RouteDefinition } from '../http/router.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import type { Clock, EventBus, Locks, MetricsSink, RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { JobQueue } from '../platform/jobs.js';
import type { Cipher } from '../entries/store.js';
import type { EventRecorder } from '../audit/recorder.js';
import { adminOrgRoutes } from './admin_orgs.js';
import { adminUserRoutes } from './admin_users.js';
import { authRoutes } from './auth.js';
import { keyRoutes } from './keys.js';
import { orgRoutes } from './orgs.js';
import { boundTokenRoutes } from './bound_tokens.js';
import { sandboxRoutes } from './sandboxes.js';
import { secretRoutes } from './secrets.js';
import { varRoutes } from './vars.js';
import { workspaceRoutes } from './workspaces.js';
import { syncRoutes } from './sync.js';
import { recloneRoutes } from './reclone.js';
import { branchPushRoutes } from './branch_push.js';
import { patchRoutes } from './patches.js';
import { rebuildRoutes } from './rebuilds.js';
import { rerereRoutes } from './rerere.js';
import { patchStatusRoutes } from './patch_status.js';
import { mergeRoutes } from './merges.js';
import { sessionRoutes } from './sessions.js';
import { costRoutes } from './cost.js';
import { ingestRoutes } from './ingest.js';
import { postmortemRoutes } from './postmortem.js';
import { runReadRoutes } from './run_reads.js';
import { auditQueryRoutes } from './audit_query.js';
import { transcriptRoutes } from './transcript.js';
import { forgeStatusRoutes } from './forge_status.js';
import { forgeReadRoutes } from './forge_reads.js';
import { forgeWriteRoutes } from './forge_writes.js';
import { webhookRoutes } from './webhook.js';
import { deliveryRoutes } from './deliveries.js';
import { lastDeliveryAt } from '../webhook/log.js';
import { eventStreamRoutes } from './events_stream.js';
import { rebaseRoutes } from './rebase.js';
import { tokenPermissionRoutes } from './token_permissions.js';
import { tokenRoutes } from './tokens.js';
import { userRoutes } from './user.js';
import { serverRoutes } from './server.js';
import { gitRoutes } from '../git/routes.js';
import type { Decider } from '../git/push.js';

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
  /** Absent where the deployment was given no repository store. */
  repos?: RepositoryStore;
  jobs?: JobQueue;
  /** Injected so a test reaches a remote of its own. */
  fetch?: typeof fetch;
  /** The deployment's admin token, which a git client may also present. */
  adminToken?: string;
  /** What the service makes of each ref of a git push. */
  decideRef?: Decider;
  /** Where the routes write down what they did. */
  events?: EventRecorder;
  /** Where a recorded event is published for the streams watching. */
  bus?: EventBus;
  /** How many event streams this deployment serves at once. */
  streamLimit?: number;
}

/**
 * Every route the service serves, in one place, so that a target wires the
 * whole surface rather than remembering to add each new group to its own
 * list.
 */
export function serviceRoutes(config: ServiceRouteConfig): RouteDefinition[] {
  return [
    // At the server root rather than under the mount point: a clone URL
    // is what a person types, and /api/v1 has no business in it.
    ...gitRoutes({
      store: config.store,
      clock: config.clock,
      ...(config.events === undefined ? {} : { events: config.events }),
      ...(config.adminToken === undefined ? {} : { adminToken: config.adminToken }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.decideRef === undefined ? {} : { decide: config.decideRef }),
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    }),
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
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      ...(config.secretCipher === undefined ? {} : { cipher: config.secretCipher }),
    }),
    ...varRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
    }),
    ...boundTokenRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
    }),
    ...rebaseRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.repos === undefined ? {} : { repos: config.repos }),
    }),
    ...(config.bus === undefined
      ? []
      : eventStreamRoutes({
          mountPoint: config.mountPoint,
          store: config.store,
          clock: config.clock,
          bus: config.bus,
          metrics: config.metrics,
          ...(config.streamLimit === undefined ? {} : { limit: config.streamLimit }),
        })),
    ...deliveryRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...webhookRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      ...(config.events === undefined ? {} : { events: config.events }),
    }),
    ...forgeWriteRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
      ...(config.events === undefined ? {} : { events: config.events }),
    }),
    ...forgeReadRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
      ...(config.events === undefined ? {} : { events: config.events }),
    }),
    ...forgeStatusRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      lastDeliveryAt: async (slug) => lastDeliveryAt(config.store, slug),
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
      ...(config.events === undefined ? {} : { events: config.events }),
    }),
    ...transcriptRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...auditQueryRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...runReadRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...postmortemRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...ingestRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.bus === undefined ? {} : { bus: config.bus }),
    }),
    ...costRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...sessionRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
    }),
    ...mergeRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
    }),
    ...patchStatusRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      ...(config.repos === undefined ? {} : { repos: config.repos }),
    }),
    ...rerereRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      locks: config.locks,
    }),
    ...rebuildRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
    }),
    ...patchRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    }),
    ...branchPushRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    }),
    ...recloneRoutes({
      mountPoint: config.mountPoint,
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    }),
    ...syncRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      locks: config.locks,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    }),
    ...workspaceRoutes({
      mountPoint: config.mountPoint,
      ...(config.events === undefined ? {} : { events: config.events }),
      store: config.store,
      clock: config.clock,
      ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
      ...(config.externalUrl === undefined ? {} : { externalUrl: config.externalUrl }),
      ...(config.repos === undefined ? {} : { repos: config.repos }),
      ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
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
