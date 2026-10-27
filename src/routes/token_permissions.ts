import { checkedScopes } from '../auth/scopes.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { setPermissions } from '../store/identity.js';
import { callerOrNotFound } from './user.js';
import { checkedGrant, ownTokenOrNotFound, requireTokenScope, tokenMetadata } from './tokens.js';

export interface TokenPermissionRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** The set the verb leaves behind, given what the token holds now. */
function applied(verb: string, held: readonly string[], named: readonly string[]): string[] {
  if (verb === 'PUT') {
    return [...named];
  }
  if (verb === 'PATCH') {
    return [...held, ...named.filter((scope) => !held.includes(scope))];
  }
  // Removing a scope the token never held is a success that changes nothing.
  return held.filter((scope) => !named.includes(scope));
}

export function tokenPermissionRoutes(config: TokenPermissionRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/user/tokens/:token_id/permissions`,
      methods: ['PUT', 'PATCH', 'DELETE'],
      handler: async (c) => {
        requireTokenScope(c, 'tokens:write');
        const user = await callerOrNotFound(c, config.store);
        const token = await ownTokenOrNotFound(c, config.store, user.id);
        if (token.revoked_at !== null) {
          throw new HttpError(400, 'this token is revoked');
        }

        const body = await readJsonObject(c);
        if (body['permissions'] === undefined || body['permissions'] === null) {
          throw new HttpError(400, 'permissions must be present');
        }
        const named = checkedScopes(body['permissions']);
        const verb = c.req.method;

        // An empty list means "revoke" on PUT and nothing coherent on the
        // other two, so only PUT accepts it.
        if (named.length === 0) {
          if (verb !== 'PUT') {
            throw new HttpError(400, `permissions must name at least one scope on ${verb}`);
          }
          const at = config.clock.now().toISOString();
          await setPermissions(config.store, token.id, [], at);
          return jsonResponse({ ...tokenMetadata(token), permissions: [], revoked_at: at });
        }

        if (verb !== 'DELETE') {
          checkedGrant(c, named);
        }
        const permissions = applied(verb, token.permissions, named);
        await setPermissions(config.store, token.id, permissions, null);

        return jsonResponse({ ...tokenMetadata(token), permissions });
      },
    },
  ];
}
