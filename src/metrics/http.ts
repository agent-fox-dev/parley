import type { Clock } from '../platform/clock.js';
import type { MetricsSink } from '../platform/metrics.js';
import { UNMATCHED_ROUTE_LABEL } from './registry.js';

/**
 * Records one request against the HTTP metrics. The path label is the route
 * template the request matched, and every request that matched no route is
 * counted under one fixed value, so no caller can grow the label set.
 */
export function recordRequest(
  metrics: MetricsSink,
  options: { method: string; routeTemplate: string | undefined; status: number; seconds: number },
): void {
  const path = options.routeTemplate ?? UNMATCHED_ROUTE_LABEL;

  metrics
    .counter('afhub_http_requests_total')
    .increment({ method: options.method, path, status: String(options.status) });

  metrics
    .histogram('afhub_http_request_duration_seconds')
    .observe({ method: options.method, path }, options.seconds);
}

/** Seconds elapsed between two readings of the clock, never negative. */
export function elapsedSeconds(clock: Clock, startedAt: Date): number {
  return Math.max(0, (clock.now().getTime() - startedAt.getTime()) / 1000);
}
