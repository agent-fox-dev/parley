import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { findUser, updateFullName } from '../store/identity.js';
import type { UserRecord } from '../store/identity.js';
import type { Context } from 'hono';

export interface UserRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/**
 * The user behind the credential. An admin token has none of its own, so a
 * route about "the caller" has nothing to answer with.
 */
export async function callerOrNotFound(c: Context, store: RelationalStore): Promise<UserRecord> {
  const userId = identityOf(c)?.userId;
  const user = userId === undefined ? undefined : await findUser(store, userId);
  if (user === undefined) {
    throw new HttpError(404, 'user not found');
  }
  return user;
}

export function userRoutes(config: UserRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/user`,
      methods: ['GET', 'PATCH'],
      scope: 'users:read',
      handler: async (c) => {
        const user = await callerOrNotFound(c, config.store);
        if (c.req.method === 'GET') {
          return jsonResponse(user);
        }

        const body = await readJsonObject(c);
        const fullName = body['full_name'];
        if (typeof fullName !== 'string' || fullName.length === 0) {
          throw new HttpError(400, 'full_name is required');
        }
        const at = config.clock.now().toISOString();
        await updateFullName(config.store, user.id, fullName, at);
        return jsonResponse({ ...user, full_name: fullName, updated_at: at });
      },
    },
  ];
}
