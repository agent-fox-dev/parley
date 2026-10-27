import { HttpError } from '../http/errors.js';
import { jsonResponse, readJsonObject, requiredString } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { formatApiKey, hashSecret, newIdentifier, newSecret } from '../auth/credentials.js';
import type { OAuthProvider, ProviderDirectory, ProviderIdentity } from '../auth/oauth.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  type CredentialRecord,
  type UserRecord,
  addMember,
  findUserByProviderIdentity,
  findUserByUsername,
  insertOrg,
  insertUser,
  orgsOfUser,
  replaceApiKeys,
} from '../store/identity.js';

export interface AuthRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  providers: ProviderDirectory;
}

/** The lifetimes a key may be issued with, in days. Zero never expires. */
const LIFETIMES = new Set([0, 30, 60, 90]);
const DEFAULT_LIFETIME = 90;
const DAY_MS = 24 * 60 * 60 * 1000;

function lifetimeDays(value: unknown): number {
  if (value === undefined || value === null) {
    return DEFAULT_LIFETIME;
  }
  if (typeof value !== 'number' || !LIFETIMES.has(value)) {
    throw new HttpError(400, 'expires must be 0, 30, 60 or 90 days');
  }
  return value;
}

function expiryOf(days: number, now: Date): string | null {
  return days === 0 ? null : new Date(now.getTime() + days * DAY_MS).toISOString();
}

function checkedRedirect(provider: OAuthProvider, body: Record<string, unknown>): void {
  const requested = body['redirect_uri'];
  if (requested === undefined || requested === null) {
    return;
  }
  if (
    typeof requested !== 'string' ||
    (provider.redirectAllowlist.length > 0 && !provider.redirectAllowlist.includes(requested))
  ) {
    throw new HttpError(400, 'redirect_uri is not on the provider allowlist');
  }
}

/**
 * The user behind a provider identity, created on first sign-in together
 * with their personal organization. A username another identity already
 * holds is a conflict, not a silent takeover.
 */
async function signedInUser(
  config: AuthRouteConfig,
  identity: ProviderIdentity,
  now: Date,
): Promise<UserRecord> {
  const existing = await findUserByProviderIdentity(
    config.store,
    identity.provider,
    identity.provider_id,
  );
  const sameName = await findUserByUsername(config.store, identity.username);

  if (existing !== undefined) {
    if (existing.status === 'blocked') {
      throw new HttpError(403, 'this user is blocked');
    }
    if (sameName !== undefined && sameName.id !== existing.id) {
      throw new HttpError(409, 'that username belongs to another identity');
    }
    return existing;
  }

  if (sameName !== undefined) {
    throw new HttpError(409, 'that username belongs to another identity');
  }

  const stamp = now.toISOString();
  const user: UserRecord = {
    id: crypto.randomUUID(),
    username: identity.username,
    email: identity.email,
    full_name: identity.full_name,
    status: 'active',
    role: 'user',
    provider: identity.provider,
    provider_id: identity.provider_id,
    created_at: stamp,
    updated_at: stamp,
  };
  await insertUser(config.store, user);

  // Their own organization, so a workspace they create has an owner from
  // the first request.
  if ((await orgsOfUser(config.store, user.id)).length === 0) {
    const org = {
      id: crypto.randomUUID(),
      name: identity.username,
      slug: identity.username,
      url: '',
      owner_id: user.id,
      status: 'active' as const,
      created_at: stamp,
      updated_at: stamp,
    };
    await insertOrg(config.store, org);
    await addMember(config.store, {
      org_id: org.id,
      user_id: user.id,
      role: 'owner',
      created_at: stamp,
    });
  }

  return user;
}

export function authRoutes(config: AuthRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/auth/providers`,
      handler: () =>
        jsonResponse(
          config.providers.list().map((provider) => ({
            name: provider.name,
            authorize_url: provider.authorizeUrl,
          })),
          200,
          { 'cache-control': 'public, max-age=300' },
        ),
    },
    {
      path: `${config.mountPoint}/auth/callback`,
      methods: ['POST'],
      handler: async (c) => {
        const body = await readJsonObject(c);
        const provider = config.providers.find(requiredString(body, 'provider'));
        if (provider === undefined) {
          throw new HttpError(400, 'that provider is not configured');
        }
        const code = requiredString(body, 'code');
        checkedRedirect(provider, body);
        const days = lifetimeDays(body['expires']);

        const identity = await provider.exchange(
          code,
          typeof body['redirect_uri'] === 'string' ? body['redirect_uri'] : undefined,
        );
        const now = config.clock.now();
        const user = await signedInUser(config, identity, now);

        const id = newIdentifier();
        const secret = newSecret();
        const issued: CredentialRecord = {
          id,
          kind: 'api_key',
          user_id: user.id,
          name: '',
          secret_hash: await hashSecret(secret),
          permissions: [],
          created_at: now.toISOString(),
          expires_at: expiryOf(days, now),
          revoked_at: null,
        };
        await replaceApiKeys(config.store, issued, now.toISOString());

        // The only place a key is ever returned.
        return jsonResponse({
          user,
          api_key: { key: formatApiKey(id, secret), key_id: id, expires_at: issued.expires_at },
        });
      },
    },
  ];
}
