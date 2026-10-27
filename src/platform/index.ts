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
