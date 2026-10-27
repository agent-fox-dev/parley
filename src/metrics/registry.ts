/**
 * The bucket boundaries the API reference fixes for the request duration
 * histogram. A scraper reads them for the life of the deployment, so they
 * are a contract and not a tuning knob.
 */
export const HTTP_DURATION_BUCKETS: readonly number[] = [
  0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
];

export interface DocumentedMetric {
  name: string;
  type: 'counter' | 'gauge' | 'histogram';
  labels: readonly string[];
  help: string;
  buckets?: readonly number[];
}

/**
 * Every metric the API reference's table names, with the type and the label
 * names it gives each. A metric this service has no source for yet is
 * exposed with the value 0 rather than omitted, so a scraper's queries do
 * not start failing when it gains one.
 */
export const DOCUMENTED_METRICS: readonly DocumentedMetric[] = [
  {
    name: 'afhub_http_requests_total',
    type: 'counter',
    labels: ['method', 'path', 'status'],
    help: 'HTTP requests',
  },
  {
    name: 'afhub_http_request_duration_seconds',
    type: 'histogram',
    labels: ['method', 'path'],
    help: 'Request latency',
    buckets: HTTP_DURATION_BUCKETS,
  },
  {
    name: 'afhub_agent_sessions_active',
    type: 'gauge',
    labels: ['workspace'],
    help: 'Active agent sessions',
  },
  {
    name: 'afhub_agent_tokens_total',
    type: 'counter',
    labels: ['workspace', 'model', 'direction'],
    help: 'Token usage',
  },
  {
    name: 'afhub_audit_events_total',
    type: 'counter',
    labels: ['source', 'event_type'],
    help: 'Audit events ingested',
  },
  {
    name: 'afhub_audit_table_rows',
    type: 'gauge',
    labels: ['table'],
    help: 'Rows per audit table',
  },
  {
    name: 'afhub_retention_errors_total',
    type: 'counter',
    labels: ['step'],
    help: 'Failed retention steps',
  },
  {
    name: 'afhub_retention_last_run_timestamp_seconds',
    type: 'gauge',
    labels: [],
    help: 'Unix time of the last successful retention run',
  },
  {
    name: 'afhub_sse_connections',
    type: 'gauge',
    labels: [],
    help: 'Open event streams',
  },
  {
    name: 'afhub_jobqueue_depth',
    type: 'gauge',
    labels: ['type', 'status'],
    help: 'Queued jobs',
  },
  {
    name: 'afhub_sandboxes_active',
    type: 'gauge',
    labels: ['workspace'],
    help: 'Sandbox leases that are active, per workspace',
  },
  {
    name: 'afhub_sandbox_creates_total',
    type: 'counter',
    labels: ['result'],
    help: 'Sandbox creates the gateway accepted',
  },
  {
    name: 'afhub_sandbox_lease_seconds',
    type: 'histogram',
    labels: [],
    help: 'Seconds from issue to end of a lease that became destroyed',
    buckets: [60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800],
  },
];

/** The one path label every request that matched no route is counted under. */
export const UNMATCHED_ROUTE_LABEL = '<unmatched>';
