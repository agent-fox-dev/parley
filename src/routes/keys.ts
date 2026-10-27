import { identityOf } from '../auth/middleware.js';
import { formatApiKey, hashSecret, newSecret } from '../auth/credentials.js';
import { conditionalJson } from '../http/etag.js';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  type CredentialRecord,
  credentialsOfUser,
  findCredential,
  revokeCredential,
  rotateSecret,
} from '../store/identity.js';
import { callerOrNotFound } from './user.js';
import type { Context } from 'hono';

export interface KeyRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** What a key listing shows: never a secret, never its hash. */
export function keyMetadata(credential: CredentialRecord) {
  return {
    key_id: credential.id,
    created_at: credential.created_at,
    expires_at: credential.expires_at,
    revoked_at: credential.revoked_at,
  };
}

/**
 * The caller's key of that identifier. A key of another user is answered
 * exactly as one that does not exist, so a caller cannot discover which
 * identifiers are in use.
 */
async function ownKeyOrNotFound(
  c: Context,
  store: RelationalStore,
  userId: string,
): Promise<CredentialRecord> {
  const key = await findCredential(store, 'api_key', c.req.param('key_id') ?? '');
  if (key === undefined || key.user_id !== userId) {
    throw new HttpError(404, 'key not found');
  }
  return key;
}

const DAY_MS = 24 * 60 * 60 * 1000;

/** The span a key was issued for, so a rotation grants the same again. */
function renewedExpiry(key: CredentialRecord, now: Date): string | null {
  if (key.expires_at === null) {
    return null;
  }
  const lifetime = Date.parse(key.expires_at) - Date.parse(key.created_at);
  return new Date(now.getTime() + Math.max(lifetime, DAY_MS)).toISOString();
}

export function keyRoutes(config: KeyRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/user/keys`,
      scope: 'keys:read',
      handler: async (c) => {
        const user = await callerOrNotFound(c, config.store);
        const keys = await credentialsOfUser(config.store, user.id, 'api_key');
        return conditionalJson(c, keys.map(keyMetadata));
      },
    },
    {
      path: `${config.mountPoint}/user/keys/:key_id/refresh`,
      methods: ['POST'],
      handler: async (c) => {
        // Rotating a key is something only a key may do: a token that could
        // would be able to mint a credential stronger than itself.
        if (identityOf(c)?.kind !== 'api_key') {
          throw new HttpError(401, 'authentication required');
        }
        const user = await callerOrNotFound(c, config.store);
        const key = await ownKeyOrNotFound(c, config.store, user.id);

        const now = config.clock.now();
        if (key.revoked_at !== null) {
          throw new HttpError(400, 'this key is revoked');
        }
        if (key.expires_at !== null && Date.parse(key.expires_at) <= now.getTime()) {
          throw new HttpError(400, 'this key has expired');
        }

        const secret = newSecret();
        const expiresAt = renewedExpiry(key, now);
        await rotateSecret(config.store, key.id, await hashSecret(secret), expiresAt);

        return jsonResponse({
          key: formatApiKey(key.id, secret),
          key_id: key.id,
          expires_at: expiresAt,
        });
      },
    },
    {
      path: `${config.mountPoint}/user/keys/:key_id`,
      methods: ['DELETE'],
      scope: 'keys:manage',
      handler: async (c) => {
        const user = await callerOrNotFound(c, config.store);
        const key = await ownKeyOrNotFound(c, config.store, user.id);
        // A key may revoke itself: the request was authenticated before the
        // handler ran, so it finishes on the credential it just retired.
        await revokeCredential(config.store, key.id, config.clock.now().toISOString());
        return emptyResponse(204);
      },
    },
  ];
}
