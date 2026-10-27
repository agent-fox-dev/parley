import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { conditionalJson } from '../http/etag.js';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse, readJsonObject, requiredString } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  type UserRecord,
  countActiveAdmins,
  credentialsOfUser,
  deleteUser,
  findCredential,
  findUser,
  findUserByProviderIdentity,
  findUserByUsername,
  insertUser,
  listUsers,
  revokeCredential,
  setUserRole,
  setUserStatus,
  updateFullName,
} from '../store/identity.js';
import { callerIsAdmin } from './orgs.js';
import { keyMetadata } from './keys.js';
import { tokenMetadata } from './tokens.js';

export interface AdminRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/**
 * Administration is for the deployment's own token and for an API key of a
 * user holding the admin role. Every other accepted credential is refused,
 * however many scopes it holds, because a scope is not a role.
 */
export async function requireAdmin(c: Context, store: RelationalStore): Promise<void> {
  if (!(await callerIsAdmin(identityOf(c), store))) {
    throw new HttpError(403, 'this route is for administrators');
  }
}

async function namedUser(c: Context, store: RelationalStore): Promise<UserRecord> {
  const user = await findUser(store, c.req.param('id') ?? '');
  if (user === undefined) {
    throw new HttpError(404, 'user not found');
  }
  return user;
}

/** Refuses a change that would leave the deployment with nobody to run it. */
async function keepOneAdmin(store: RelationalStore, user: UserRecord): Promise<void> {
  if (user.role === 'admin' && user.status === 'active' && (await countActiveAdmins(store)) <= 1) {
    throw new HttpError(409, 'this is the last active admin');
  }
}

export function adminUserRoutes(config: AdminRouteConfig): RouteDefinition[] {
  const { store, clock } = config;
  const base = `${config.mountPoint}/users`;

  const statusRoute = (suffix: string, status: 'active' | 'blocked'): RouteDefinition => ({
    path: `${base}/:id/${suffix}`,
    methods: ['POST'],
    handler: async (c) => {
      await requireAdmin(c, store);
      const user = await namedUser(c, store);
      const at = clock.now().toISOString();
      await setUserStatus(store, user.id, status, at);
      return jsonResponse({ ...user, status, updated_at: at });
    },
  });

  const roleRoute = (suffix: string, role: 'user' | 'admin'): RouteDefinition => ({
    path: `${base}/:id/${suffix}`,
    methods: ['POST'],
    handler: async (c) => {
      await requireAdmin(c, store);
      const user = await namedUser(c, store);
      if (role === 'user' && user.role === 'admin') {
        await keepOneAdmin(store, user);
      }
      // Asking for a role the user already holds is a success that changes
      // nothing, so a script may repeat itself safely.
      const at = clock.now().toISOString();
      await setUserRole(store, user.id, role, at);
      return jsonResponse({ ...user, role, updated_at: at });
    },
  });

  return [
    {
      path: base,
      methods: ['GET', 'POST'],
      handler: async (c) => {
        await requireAdmin(c, store);
        if (c.req.method === 'GET') {
          return jsonResponse(await listUsers(store));
        }

        const body = await readJsonObject(c);
        const username = requiredString(body, 'username');
        const email = requiredString(body, 'email');
        const provider = requiredString(body, 'provider');
        const providerId = requiredString(body, 'provider_id');

        if (
          (await findUserByUsername(store, username)) !== undefined ||
          (await findUserByProviderIdentity(store, provider, providerId)) !== undefined
        ) {
          throw new HttpError(409, 'that username or provider identity is taken');
        }

        const at = clock.now().toISOString();
        const user: UserRecord = {
          id: crypto.randomUUID(),
          username,
          email,
          full_name: typeof body['full_name'] === 'string' ? body['full_name'] : '',
          status: 'active',
          role: 'user',
          provider,
          provider_id: providerId,
          created_at: at,
          updated_at: at,
        };
        await insertUser(store, user);
        return jsonResponse(user, 201);
      },
    },
    {
      path: `${base}/:id`,
      methods: ['GET', 'PATCH', 'DELETE'],
      handler: async (c) => {
        await requireAdmin(c, store);
        const user = await namedUser(c, store);

        if (c.req.method === 'GET') {
          return conditionalJson(c, user);
        }
        if (c.req.method === 'DELETE') {
          await keepOneAdmin(store, user);
          await deleteUser(store, user.id);
          return emptyResponse(204);
        }

        const body = await readJsonObject(c);
        const fullName = body['full_name'];
        if (typeof fullName !== 'string' || fullName.length === 0) {
          throw new HttpError(400, 'full_name is required');
        }
        const at = clock.now().toISOString();
        await updateFullName(store, user.id, fullName, at);
        return jsonResponse({ ...user, full_name: fullName, updated_at: at });
      },
    },
    roleRoute('promote', 'admin'),
    roleRoute('demote', 'user'),
    statusRoute('block', 'blocked'),
    statusRoute('unblock', 'active'),
    {
      path: `${base}/:id/keys`,
      handler: async (c) => {
        await requireAdmin(c, store);
        const user = await namedUser(c, store);
        const keys = await credentialsOfUser(store, user.id, 'api_key');
        return jsonResponse(keys.map((key) => ({ ...keyMetadata(key), user_id: user.id })));
      },
    },
    {
      path: `${base}/:id/tokens`,
      handler: async (c) => {
        await requireAdmin(c, store);
        const user = await namedUser(c, store);
        const tokens = await credentialsOfUser(store, user.id, 'pat');
        return jsonResponse(tokens.map((token) => ({ ...tokenMetadata(token), user_id: user.id })));
      },
    },
    {
      path: `${base}/:id/keys/:key_id`,
      methods: ['DELETE'],
      handler: async (c) => revokeOf(c, config, 'api_key', 'key_id'),
    },
    {
      path: `${base}/:id/tokens/:token_id`,
      methods: ['DELETE'],
      handler: async (c) => revokeOf(c, config, 'pat', 'token_id'),
    },
  ];
}

async function revokeOf(
  c: Context,
  config: AdminRouteConfig,
  kind: 'api_key' | 'pat',
  param: string,
): Promise<Response> {
  await requireAdmin(c, config.store);
  const user = await namedUser(c, config.store);
  const credential = await findCredential(config.store, kind, c.req.param(param) ?? '');
  if (credential === undefined || credential.user_id !== user.id) {
    throw new HttpError(404, 'credential not found');
  }
  await revokeCredential(config.store, credential.id, config.clock.now().toISOString());
  return emptyResponse(204);
}
