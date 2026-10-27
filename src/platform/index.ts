export type { Clock } from './clock.js';
export type { ConfigSource } from './config.js';
export type { CounterHandle, HistogramHandle, MetricsSink } from './metrics.js';
export type { RelationalStore } from './store.js';

import type { Clock } from './clock.js';
import type { ConfigSource } from './config.js';
import type { MetricsSink } from './metrics.js';
import type { RelationalStore } from './store.js';

/**
 * Everything the service reaches its host platform through. A target
 * assembles one of these and hands it to the service; nothing in the
 * service reaches a platform API directly.
 */
export interface Platform {
  store: RelationalStore;
  clock: Clock;
  config: ConfigSource;
  metrics: MetricsSink;
}

/**
 * The configured request body limit, where the deployment set a usable one.
 * Anything unreadable leaves the service's own default in place.
 */
export function maxBodyBytesFrom(config: ConfigSource): { maxBodyBytes?: number } {
  const raw = config.get('PARLEY_MAX_BODY_BYTES');
  if (raw === undefined) {
    return {};
  }
  const value = Number(raw);
  return Number.isInteger(value) && value > 0 ? { maxBodyBytes: value } : {};
}
