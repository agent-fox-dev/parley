import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { formatPat, hashSecret, newIdentifier, newSecret } from '../auth/credentials.js';
import { grantsScope, checkedScopes } from '../auth/scopes.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  type CredentialRecord,
  credentialsOfUser,
  findCredential,
  insertCredential,
  revokeCredential,
} from '../store/identity.js';
import { callerOrNotFound } from './user.js';

export interface TokenRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

const MAX_NAME = 255;
const LIFETIMES = new Set([0, 30, 60, 90]);
const DEFAULT_LIFETIME = 90;
const DAY_MS = 24 * 60 * 60 * 1000;

/** What a token looks like everywhere but the one route that creates it. */
export function tokenMetadata(token: CredentialRecord) {
  return {
    token_id: token.id,
    name: token.name,
    permissions: token.permissions,
    expires_at: token.expires_at,
    created_at: token.created_at,
    revoked_at: token.revoked_at,
  };
}

export function tokenExpiry(value: unknown, now: Date): string | null {
  const days = value === undefined || value === null ? DEFAULT_LIFETIME : value;
  if (typeof days !== 'number' || !LIFETIMES.has(days)) {
    throw new HttpError(400, 'expires must be 0, 30, 60 or 90 days');
  }
  return days === 0 ? null : new Date(now.getTime() + days * DAY_MS).toISOString();
}

export function checkedName(body: Record<string, unknown>): string {
  const name = body['name'];
  if (typeof name !== 'string' || name.length === 0 || name.length > MAX_NAME) {
    throw new HttpError(400, `name is required and may be at most ${MAX_NAME} characters`);
  }
  return name;
}

/**
 * A caller may not hand out more than it holds. API keys and admin tokens
 * hold every scope, so only a token is measured against what it asks for.
 */
export function checkedGrant(c: Context, requested: readonly string[]): void {
  const identity = identityOf(c);
  if (identity?.kind !== 'pat') {
    return;
  }
  for (const scope of requested) {
    if (!grantsScope(identity.permissions, scope)) {
      throw new HttpError(403, `this token does not hold ${scope}`);
    }
  }
}

/**
 * The caller's token of that identifier, or a 404 that says nothing about
 * whether it exists for somebody else.
 */
export async function ownTokenOrNotFound(
  c: Context,
  store: RelationalStore,
  userId: string,
): Promise<CredentialRecord> {
  const token = await findCredential(store, 'pat', c.req.param('token_id') ?? '');
  if (token === undefined || token.user_id !== userId) {
    throw new HttpError(404, 'token not found');
  }
  return token;
}

export function tokenRoutes(config: TokenRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/user/tokens`,
      methods: ['GET', 'POST'],
      handler: async (c) => {
        const user = await callerOrNotFound(c, config.store);

        if (c.req.method === 'GET') {
          requireTokenScope(c, 'tokens:read');
          const held = await credentialsOfUser(config.store, user.id, 'pat');
          return jsonResponse(held.map(tokenMetadata));
        }

        requireTokenScope(c, 'tokens:manage');
        const minted = await mintToken(c, config.store, config.clock, user.id);

        // The only place an unbound token string is ever returned.
        return jsonResponse({ ...tokenMetadata(minted.token), token: minted.presented }, 201);
      },
    },
    {
      path: `${config.mountPoint}/user/tokens/:token_id`,
      methods: ['GET', 'DELETE'],
      handler: async (c) => {
        const user = await callerOrNotFound(c, config.store);

        if (c.req.method === 'GET') {
          requireTokenScope(c, 'tokens:read');
          return jsonResponse(tokenMetadata(await ownTokenOrNotFound(c, config.store, user.id)));
        }

        requireTokenScope(c, 'tokens:manage');
        const token = await ownTokenOrNotFound(c, config.store, user.id);
        if (token.revoked_at !== null) {
          throw new HttpError(400, 'this token is already revoked');
        }
        const at = config.clock.now().toISOString();
        await revokeCredential(config.store, token.id, at);

        // The reference answers this one with the metadata, not 204.
        return jsonResponse({ ...tokenMetadata(token), revoked_at: at });
      },
    },
  ];
}

export interface MintedToken {
  token: CredentialRecord;
  /** The token string, which exists only here and in the response. */
  presented: string;
}

/**
 * Creates a token from the request's body, with the validation, expiry and
 * privilege rules the reference gives. The bound-token route mints with
 * exactly this, and binds what it gets.
 */
export async function mintToken(
  c: Context,
  store: RelationalStore,
  clock: Clock,
  userId: string,
): Promise<MintedToken> {
  const body = await readJsonObject(c);
  const name = checkedName(body);
  const permissions = checkedScopes(body['permissions']);
  if (permissions.length === 0) {
    throw new HttpError(400, 'permissions must name at least one scope');
  }
  const now = clock.now();
  const expiresAt = tokenExpiry(body['expires'], now);
  checkedGrant(c, permissions);

  const id = newIdentifier();
  const secret = newSecret();
  const token: CredentialRecord = {
    id,
    kind: 'pat',
    user_id: userId,
    name,
    secret_hash: await hashSecret(secret),
    permissions,
    created_at: now.toISOString(),
    expires_at: expiresAt,
    revoked_at: null,
  };
  await insertCredential(store, token);
  return { token, presented: formatPat(id, secret) };
}

/**
 * The scope check for a route that answers several methods, where the
 * router's own declarative check cannot tell them apart.
 */
export function requireTokenScope(c: Context, scope: string): void {
  const identity = identityOf(c);
  if (identity?.kind === 'pat' && !grantsScope(identity.permissions, scope)) {
    throw new HttpError(403, `this token does not hold ${scope}`);
  }
}
