import { Hono } from 'hono';
import type { Context } from 'hono';
import { DEFAULT_MAX_BODY_BYTES, DEFAULT_MOUNT_POINT } from '../config.js';
import { serverRoutes } from '../routes/server.js';
import { HttpError, errorResponse } from './errors.js';

export { DEFAULT_MOUNT_POINT } from '../config.js';

/** Where an unhandled failure's detail goes. It never reaches a response body. */
export interface LogSink {
  error(message: string, detail?: unknown): void;
}

export interface RouteDefinition {
  path: string;
  handler: () => Response | Promise<Response>;
  /** The methods this route answers. Defaults to GET alone. */
  methods?: readonly string[];
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
}

const consoleLog: LogSink = {
  error(message, detail) {
    console.error(message, detail);
  },
};

export function buildApp(options: AppOptions = {}): Hono {
  const mountPoint = options.mountPoint ?? DEFAULT_MOUNT_POINT;
  const log = options.log ?? consoleLog;
  const routes =
    options.routes ??
    serverRoutes({
      mountPoint,
      buildTime: options.buildTime ?? '',
      commit: options.commit ?? '',
    });

  const maxBodyBytes = options.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;

  const app = new Hono();

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

  for (const route of routes) {
    for (const method of route.methods ?? ['GET']) {
      app.on(method, route.path, () => route.handler());
    }
    // Registered after the route's own methods, so only another one reaches it.
    app.all(route.path, () =>
      errorResponse(new HttpError(405, `method not allowed on ${route.path}`)),
    );
  }

  app.notFound(() => errorResponse(new HttpError(404, 'not found')));

  app.onError((err) => {
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
