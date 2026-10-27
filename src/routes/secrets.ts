import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { type ScopeFamily, entryScope } from '../entries/scopes.js';
import { asSecret } from '../entries/serialise.js';
import {
  type Cipher,
  createEntries,
  deleteEntry,
  listEntries,
  replaceEntry,
} from '../entries/store.js';
import { checkedEntries, checkedValue } from '../entries/validate.js';

export interface SecretRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  /** Absent where the deployment configured no key to seal values with. */
  cipher?: Cipher;
}

/** The three scopes a secret can live at, and the path each is served under. */
const FAMILIES: ReadonlyArray<{ family: ScopeFamily; path: string }> = [
  { family: 'user', path: '/user/secrets' },
  { family: 'org', path: '/orgs/:slug/secrets' },
  { family: 'workspace', path: '/workspaces/:slug/secrets' },
];

function cipherOf(config: SecretRouteConfig): Cipher {
  if (config.cipher === undefined) {
    throw new HttpError(500, 'this deployment has no key to seal secrets with');
  }
  return config.cipher;
}

/** A workspace's secrets are private: a member of its organization is refused. */
const NEED = 'private' as const;

async function listSecrets(
  c: Context,
  config: SecretRouteConfig,
  family: ScopeFamily,
): Promise<Response> {
  const scope = await entryScope(c, config.store, family, NEED);
  const entries = await listEntries(config.store, scope, 'secret', cipherOf(config));
  return jsonResponse(entries.map(asSecret));
}

async function createSecrets(
  c: Context,
  config: SecretRouteConfig,
  family: ScopeFamily,
): Promise<Response> {
  const scope = await entryScope(c, config.store, family, NEED);
  const inputs = checkedEntries(await readJsonObject(c));
  const created = await createEntries(
    config.store,
    scope,
    'secret',
    inputs,
    config.clock.now().toISOString(),
    cipherOf(config),
  );
  return jsonResponse(created.map(asSecret), 201);
}

async function replaceSecret(
  c: Context,
  config: SecretRouteConfig,
  family: ScopeFamily,
): Promise<Response> {
  const scope = await entryScope(c, config.store, family, NEED);
  const value = checkedValue((await readJsonObject(c))['value']);
  const replaced = await replaceEntry(
    config.store,
    scope,
    'secret',
    c.req.param('key') ?? '',
    value,
    config.clock.now().toISOString(),
    cipherOf(config),
  );
  return jsonResponse(asSecret(replaced));
}

async function removeSecret(
  c: Context,
  config: SecretRouteConfig,
  family: ScopeFamily,
): Promise<Response> {
  const scope = await entryScope(c, config.store, family, NEED);
  await deleteEntry(config.store, scope, 'secret', c.req.param('key') ?? '');
  return emptyResponse(204);
}

export function secretRoutes(config: SecretRouteConfig): RouteDefinition[] {
  return FAMILIES.flatMap(({ family, path }) => [
    {
      path: `${config.mountPoint}${path}`,
      scope: 'secrets:read',
      handler: async (c: Context) => listSecrets(c, config, family),
    },
    {
      path: `${config.mountPoint}${path}`,
      methods: ['POST'],
      scope: 'secrets:write',
      handler: async (c: Context) => createSecrets(c, config, family),
    },
    {
      path: `${config.mountPoint}${path}/:key`,
      methods: ['PATCH'],
      scope: 'secrets:write',
      handler: async (c: Context) => replaceSecret(c, config, family),
    },
    {
      path: `${config.mountPoint}${path}/:key`,
      methods: ['DELETE'],
      scope: 'secrets:write',
      handler: async (c: Context) => removeSecret(c, config, family),
    },
  ]);
}
