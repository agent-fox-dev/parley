import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { workspaceAccess } from '../workspace/access.js';
import { listDeliveries } from '../webhook/log.js';
import type { ForgeRouteConfig } from './forge.js';

/**
 * The log of what the forge sent. It is read when something did not
 * happen that should have, so every parameter it cannot read is
 * named in the refusal rather than quietly ignored.
 */
export type DeliveryRouteConfig = ForgeRouteConfig;

/** How many rows one read brings back when the caller asks for none. */
const BY_DEFAULT = 30;
const MOST = 100;

/** One value of a parameter, refused where it is given twice. */
function once(c: Context, name: string): string | undefined {
  const given = new URL(c.req.url).searchParams.getAll(name);
  if (given.length > 1) {
    throw new HttpError(400, `${name} must be given once`);
  }
  return given[0];
}

function readDeliveryQuery(c: Context): { since?: string; limit: number } {
  const asked = once(c, 'limit');
  let limit = BY_DEFAULT;
  if (asked !== undefined) {
    const wanted = Number(asked);
    if (!Number.isInteger(wanted) || wanted < 1 || wanted > MOST) {
      throw new HttpError(400, `limit must be an integer from 1 to ${String(MOST)}`);
    }
    limit = wanted;
  }

  const given = once(c, 'since');
  if (given === undefined) {
    return { limit };
  }
  if (given.length === 0 || Number.isNaN(Date.parse(given))) {
    throw new HttpError(
      400,
      'since must be an RFC 3339 timestamp, for example 2026-10-21T09:00:00Z',
    );
  }
  // Whole seconds are kept, so a bound with a fraction in it starts
  // at the next whole second rather than halfway through one.
  const at = new Date(given);
  const seconds = Math.ceil(at.getTime() / 1000) * 1000;
  return { since: new Date(seconds).toISOString(), limit };
}

export function deliveryRoutes(config: DeliveryRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/forge/deliveries`,
      scope: 'forge:read',
      handler: async (c) => {
        const { workspace } = await workspaceAccess(c, config.store, 'read');
        const query = readDeliveryQuery(c);
        return jsonResponse(await listDeliveries(config.store, workspace.slug, query));
      },
    },
  ];
}
