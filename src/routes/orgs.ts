import { identityOf } from '../auth/middleware.js';
import { conditionalJson } from '../http/etag.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { findOrg, findUser, isMember, orgsOfUser } from '../store/identity.js';

export interface OrgRouteConfig {
  mountPoint: string;
  store: RelationalStore;
}

/** Whether the caller administers the deployment, however they authenticated. */
export async function callerIsAdmin(
  identity: ReturnType<typeof identityOf>,
  store: RelationalStore,
): Promise<boolean> {
  if (identity === undefined) {
    return false;
  }
  if (identity.kind === 'admin_token') {
    return true;
  }
  if (identity.kind !== 'api_key' || identity.userId === undefined) {
    return false;
  }
  return (await findUser(store, identity.userId))?.role === 'admin';
}

export function orgRoutes(config: OrgRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/user/orgs`,
      scope: 'orgs:read',
      handler: async (c) => {
        const userId = identityOf(c)?.userId;
        // An admin token belongs to no user, so it belongs to no organization.
        return jsonResponse(userId === undefined ? [] : await orgsOfUser(config.store, userId));
      },
    },
    {
      path: `${config.mountPoint}/orgs/:id`,
      scope: 'orgs:read',
      handler: async (c) => {
        const org = await findOrg(config.store, c.req.param('id') ?? '');
        if (org === undefined) {
          throw new HttpError(404, 'organization not found');
        }

        const identity = identityOf(c);
        const admitted =
          (await callerIsAdmin(identity, config.store)) ||
          (identity?.userId !== undefined &&
            (await isMember(config.store, org.id, identity.userId)));
        if (!admitted) {
          throw new HttpError(403, 'you are not a member of this organization');
        }

        return conditionalJson(c, org);
      },
    },
  ];
}
