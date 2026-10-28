import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { identityOf } from '../auth/middleware.js';
import { DENIED, mayReadSessions } from '../session/read.js';
import { costOf, readCostQuery } from '../session/cost.js';

/**
 * What a workspace's agents have cost. It reads like the session
 * reads do, and refuses like them: an archived workspace is told to
 * the caller rather than hidden from them.
 */
export interface CostRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

async function cost(c: Context, config: CostRouteConfig): Promise<Response> {
  const slug = c.req.param('slug') ?? '';
  const asked = readCostQuery(new URL(c.req.url).searchParams, config.clock);

  if (!(await mayReadSessions(config.store, identityOf(c), slug))) {
    throw new HttpError(403, DENIED);
  }
  return jsonResponse(await costOf(config.store, slug, asked));
}

export function costRoutes(config: CostRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/cost`,
      scope: 'sessions:read',
      handler: async (c) => cost(c, config),
    },
  ];
}
