import { Hono } from 'hono';
import { serverRoutes } from '../routes/server.js';
import { HttpError, errorResponse } from './errors.js';

/** The mount point every versioned route is served under when none is configured. */
export const DEFAULT_MOUNT_POINT = '/api/v1';

/** Where an unhandled failure's detail goes. It never reaches a response body. */
export interface LogSink {
  error(message: string, detail?: unknown): void;
}

export interface RouteDefinition {
  path: string;
  handler: () => Response | Promise<Response>;
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

  const app = new Hono();

  for (const route of routes) {
    app.get(route.path, () => route.handler());
    // Registered after the GET, so only another method reaches it.
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
