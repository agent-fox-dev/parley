import type { Clock, RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';
import type { LogSink } from '../http/router.js';
import type { EventRecorder } from '../audit/recorder.js';
import type { ForgeDeps } from '../forge/guard.js';

/**
 * What every forge route group is given. The groups differ in what
 * they do with it and not in what they need, so the config is one
 * shape and the guard's own deps are read out of it.
 */
export interface ForgeRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock?: Clock;
  /** Absent where the deployment configured no key to seal secrets with. */
  secrets?: SecretStore;
  /** The deployment's own URL, for the webhook address it advertises. */
  externalUrl?: string;
  /** Injected so a suite reaches a forge of its own. */
  fetch?: typeof fetch;
  /** Where a write writes down what it did. */
  events?: EventRecorder;
  log?: LogSink;
  /** The deployment's own API URLs, which reads may use and writes may not. */
  environment?: { github?: string; gitlab?: string };
  /** How long a call to the forge has; the reference's 30 seconds by default. */
  deadlineMs?: number;
}

/** The part of the config the guard and the client need. */
export function forgeDeps(config: ForgeRouteConfig): ForgeDeps {
  return {
    store: config.store,
    ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
    ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    ...(config.log === undefined ? {} : { log: config.log }),
    ...(config.events === undefined ? {} : { events: config.events }),
    ...(config.environment === undefined ? {} : { environment: config.environment }),
    ...(config.deadlineMs === undefined ? {} : { deadlineMs: config.deadlineMs }),
  };
}
