import type { RouteDefinition } from '../http/router.js';

/**
 * The four routes served at the server root, outside the mount point. Their
 * order is the order they are registered in.
 */
export const SERVER_ROUTE_PATHS = ['/healthz', '/readyz', '/version', '/metrics'] as const;

/** The Prometheus text exposition content type the API reference fixes. */
export const PROMETHEUS_CONTENT_TYPE = 'text/plain; version=0.0.4';

export interface ServerRouteConfig {
  mountPoint: string;
  buildTime: string;
  commit: string;
}

/**
 * The version of the JavaScript runtime the service is running on, reported
 * under a truthful key in place of the hub's `go_version`.
 */
export function runtimeVersion(): string {
  const nav = (globalThis as { navigator?: { userAgent?: string } }).navigator;
  if (typeof nav?.userAgent === 'string' && nav.userAgent.length > 0) {
    return nav.userAgent;
  }
  const proc = (globalThis as { process?: { version?: string } }).process;
  if (typeof proc?.version === 'string' && proc.version.length > 0) {
    return proc.version;
  }
  return 'unknown';
}

function json(body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json; charset=UTF-8', ...headers },
  });
}

export function serverRoutes(config: ServerRouteConfig): RouteDefinition[] {
  return [
    // Liveness answers from the process alone and reads no store.
    { path: '/healthz', handler: () => json({ status: 'ok' }) },
    // Readiness answers from the relational store once there is one to query.
    { path: '/readyz', handler: () => json({ status: 'ready' }) },
    {
      path: '/version',
      handler: () =>
        json(
          {
            build_time: config.buildTime,
            commit: config.commit,
            mount_point: config.mountPoint,
            runtime_version: runtimeVersion(),
          },
          { 'cache-control': 'public, max-age=300' },
        ),
    },
    {
      path: '/metrics',
      handler: () =>
        new Response('', { status: 200, headers: { 'content-type': PROMETHEUS_CONTENT_TYPE } }),
    },
  ];
}
