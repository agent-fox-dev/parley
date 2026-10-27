import { Hono } from 'hono';
import type { Context } from 'hono';
import { type AuthOptions, authentication, requireScope } from '../auth/middleware.js';
import { redactCredentials } from '../auth/credentials.js';
import { DEFAULT_MAX_BODY_BYTES, DEFAULT_MOUNT_POINT } from '../config.js';
import type { Clock, MetricsSink, RelationalStore } from '../platform/index.js';
import { elapsedSeconds, recordRequest } from '../metrics/http.js';
import { systemClock } from '../platform/local/clock.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import { serverRoutes } from '../routes/server.js';
import { HttpError, errorResponse } from './errors.js';

export { DEFAULT_MOUNT_POINT } from '../config.js';

/** Where an unhandled failure's detail goes. It never reaches a response body. */
export interface LogSink {
  error(message: string, detail?: unknown): void;
  /** What the service answered, for an operator reading the log. */
  info?(message: string, detail?: unknown): void;
}

export interface RouteDefinition {
  path: string;
  handler: (c: Context) => Response | Promise<Response>;
  /** The methods this route answers. Defaults to GET alone. */
  methods?: readonly string[];
  /** The scope a personal access token needs to reach this route. */
  scope?: string;
}

export interface AppOptions {
  /** The prefix the versioned routes live under. Defaults to /api/v1. */
  mountPoint?: string;
  /** Build information, empty where the build did not supply it. */
  buildTime?: string;
  commit?: string;
  log?: LogSink;
  /** The routes served at the root. Defaults to the four server routes. */
  routes?: RouteDefinition[];
  /** The largest request body accepted. Defaults to DEFAULT_MAX_BODY_BYTES. */
  maxBodyBytes?: number;
  /** The store the readiness probe queries. */
  store?: RelationalStore;
  /** Where measurements are recorded. Defaults to an in-memory sink. */
  metrics?: MetricsSink;
  /** Read to time a request. Defaults to the host's own clock. */
  clock?: Clock;
  /** Where a credential is identified. Omitted, every route is public. */
  auth?: AuthOptions;
}

const consoleLog: LogSink = {
  error(message, detail) {
    console.error(message, detail);
  },
  info(message, detail) {
    console.log(message, detail);
  },
};

/**
 * A log that cannot carry a credential, whatever a failure put in its
 * message. Every record of the service goes through it.
 */
function scrub(detail: unknown): unknown {
  if (detail === undefined) {
    return undefined;
  }
  if (detail instanceof Error) {
    // Still an Error, so a reader of the log sees the failure as it was
    // raised; only a credential inside it is replaced.
    const safe = new Error(redactCredentials(detail.message));
    safe.name = detail.name;
    safe.stack = redactCredentials(detail.stack ?? '');
    return safe;
  }
  if (typeof detail === 'string') {
    return redactCredentials(detail);
  }
  const encoded = JSON.stringify(detail);
  return encoded === undefined ? '[unserialisable]' : JSON.parse(redactCredentials(encoded));
}

function redacting(log: LogSink): LogSink {
  return {
    error: (message, detail) => log.error(redactCredentials(message), scrub(detail)),
    info: (message, detail) => log.info?.(redactCredentials(message), scrub(detail)),
  };
}

export function buildApp(options: AppOptions = {}): Hono {
  const mountPoint = options.mountPoint ?? DEFAULT_MOUNT_POINT;
  const log = redacting(options.log ?? consoleLog);
  const maxBodyBytes = options.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;
  const metrics = options.metrics ?? inMemoryMetrics();
  const clock = options.clock ?? systemClock();

  const routes =
    options.routes ??
    serverRoutes({
      mountPoint,
      buildTime: options.buildTime ?? '',
      commit: options.commit ?? '',
      ...(options.store === undefined ? {} : { store: options.store }),
      metrics,
    });

  const app = new Hono();

  // Outermost, so every answer is counted, refusals and failures included.
  app.use('*', async (c, next) => {
    const startedAt = clock.now();
    await next();
    recordRequest(metrics, {
      method: c.req.method,
      routeTemplate: c.req.routePath === '/*' ? undefined : c.req.routePath,
      status: c.res.status,
      seconds: elapsedSeconds(clock, startedAt),
    });
  });

  // Ahead of every route, so an over-sized body never reaches a handler.
  app.use('*', async (c, next) => {
    if (await bodyExceeds(c, maxBodyBytes)) {
      return errorResponse(
        new HttpError(413, `the request body is larger than the ${maxBodyBytes} bytes accepted`),
      );
    }
    await next();
    return undefined;
  });

  if (options.auth !== undefined) {
    app.use('*', authentication({ ...options.auth, log }));
  }

  for (const route of routes) {
    if (route.scope !== undefined) {
      app.use(route.path, requireScope(route.scope));
    }
  }

  for (const route of routes) {
    for (const method of route.methods ?? ['GET']) {
      app.on(method, route.path, (c) => route.handler(c));
    }
  }

  // After every route's own methods, so that two groups serving different
  // methods of one path do not shadow each other, and only a method nobody
  // answers reaches the refusal.
  for (const path of new Set(routes.map((route) => route.path))) {
    app.all(path, () => errorResponse(new HttpError(405, `method not allowed on ${path}`)));
  }

  app.notFound(() => errorResponse(new HttpError(404, 'not found')));

  app.onError((err) => {
    // A route that refused the request said what to answer; anything else
    // is a failure nobody expected, and its detail goes to the log alone.
    if (err instanceof HttpError) {
      return errorResponse(err);
    }
    log.error('unhandled failure while answering a request', err);
    return errorResponse(new HttpError(500, 'internal server error'));
  });

  return app;
}

/**
 * Whether a request's body is larger than the limit. A declared content
 * length answers without reading anything. Without one the body has to be
 * read to be measured; Hono caches it on the request, so a later handler
 * still reads the same body.
 */
async function bodyExceeds(c: Context, limit: number): Promise<boolean> {
  const declared = c.req.header('content-length');
  if (declared !== undefined) {
    const length = Number(declared);
    return Number.isFinite(length) && length > limit;
  }

  if (c.req.raw.body === null) {
    return false;
  }

  const body = await c.req.arrayBuffer();
  return body.byteLength > limit;
}
