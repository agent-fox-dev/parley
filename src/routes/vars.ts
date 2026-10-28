import { type EventRecorder, actorIn, record } from '../audit/recorder.js';
import type { Context } from 'hono';
import { emptyResponse, jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { type ScopeFamily, entryScope } from '../entries/scopes.js';
import { asVariable } from '../entries/serialise.js';
import {
  createEntries,
  deleteEntry,
  listEntries,
  plainCipher,
  replaceEntry,
} from '../entries/store.js';
import { checkedEntries, checkedValue } from '../entries/validate.js';
import { type AccessNeed, workspaceAccess } from '../workspace/access.js';
import { resolveVariables } from '../vars/resolve.js';

export interface VarRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  /** Where this group writes down what it did. */
  events?: EventRecorder;
}

const FAMILIES: ReadonlyArray<{ family: ScopeFamily; path: string }> = [
  { family: 'user', path: '/user/vars' },
  { family: 'org', path: '/orgs/:slug/vars' },
  { family: 'workspace', path: '/workspaces/:slug/vars' },
];

/**
 * Unlike a secret, a workspace's variables are read at read access and
 * changed at write access, so a member of its organization lists them and
 * is refused a change. That is the only place the two kinds diverge.
 */
const READING: AccessNeed = 'read';
const WRITING: AccessNeed = 'write';

async function listVars(c: Context, config: VarRouteConfig, family: ScopeFamily) {
  const scope = await entryScope(c, config.store, family, READING);
  const entries = await listEntries(config.store, scope, 'variable', plainCipher);
  return jsonResponse(entries.map(asVariable));
}

/** One event per key, as a secret records one, by scope and key. */
async function recordKeys(
  c: Context,
  config: VarRouteConfig,
  family: ScopeFamily,
  action: 'create' | 'update' | 'delete',
  keys: readonly string[],
): Promise<void> {
  for (const key of keys) {
    await record(config.events, {
      type: `hub.variable.${action}`,
      workspace: family === 'workspace' ? (c.req.param('slug') ?? null) : null,
      resourceId: key,
      actor: actorIn(c),
      metadata: { scope: family, key },
    });
  }
}

async function createVars(c: Context, config: VarRouteConfig, family: ScopeFamily) {
  const scope = await entryScope(c, config.store, family, WRITING);
  const inputs = checkedEntries(await readJsonObject(c));
  const created = await createEntries(
    config.store,
    scope,
    'variable',
    inputs,
    config.clock.now().toISOString(),
    plainCipher,
  );
  await recordKeys(
    c,
    config,
    family,
    'create',
    created.map((one) => one.key),
  );
  return jsonResponse(created.map(asVariable), 201);
}

async function replaceVar(c: Context, config: VarRouteConfig, family: ScopeFamily) {
  const scope = await entryScope(c, config.store, family, WRITING);
  const value = checkedValue((await readJsonObject(c))['value']);
  const replaced = await replaceEntry(
    config.store,
    scope,
    'variable',
    c.req.param('key') ?? '',
    value,
    config.clock.now().toISOString(),
    plainCipher,
  );
  await recordKeys(c, config, family, 'update', [replaced.key]);
  return jsonResponse(asVariable(replaced));
}

async function removeVar(c: Context, config: VarRouteConfig, family: ScopeFamily) {
  const scope = await entryScope(c, config.store, family, WRITING);
  await deleteEntry(config.store, scope, 'variable', c.req.param('key') ?? '');
  await recordKeys(c, config, family, 'delete', [c.req.param('key') ?? '']);
  return emptyResponse(204);
}

/**
 * The three tiers merged, at read access, so a member of the workspace's
 * organization is answered what its owner is answered.
 */
async function resolvedVars(c: Context, config: VarRouteConfig) {
  const { workspace } = await workspaceAccess(c, config.store, READING);
  return jsonResponse(await resolveVariables(config.store, workspace));
}

export function varRoutes(config: VarRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/vars/resolved`,
      scope: 'vars:read',
      handler: async (c: Context) => resolvedVars(c, config),
    },
    ...FAMILIES.flatMap(({ family, path }) => [
      {
        path: `${config.mountPoint}${path}`,
        scope: 'vars:read',
        handler: async (c: Context) => listVars(c, config, family),
      },
      {
        path: `${config.mountPoint}${path}`,
        methods: ['POST'],
        scope: 'vars:write',
        handler: async (c: Context) => createVars(c, config, family),
      },
      {
        path: `${config.mountPoint}${path}/:key`,
        methods: ['PATCH'],
        scope: 'vars:write',
        handler: async (c: Context) => replaceVar(c, config, family),
      },
      {
        path: `${config.mountPoint}${path}/:key`,
        methods: ['DELETE'],
        scope: 'vars:write',
        handler: async (c: Context) => removeVar(c, config, family),
      },
    ]),
  ];
}
