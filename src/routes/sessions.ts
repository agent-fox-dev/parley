import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { newIdentifier } from '../auth/credentials.js';
import { type Identity, identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { completeSession, ownedSession, readCompleteBody } from '../session/complete.js';
import { readUsageBody, recordUsage } from '../session/usage.js';
import {
  type SessionRecord,
  insertSession,
  serialiseSession,
  sessionById,
  tokenSummary,
} from '../session/store.js';

/**
 * Agent sessions.
 *
 * Two access rules here differ from everywhere else in the service
 * and are easy to flatten by accident. Opening a session needs
 * **write** access, so a member of the workspace's organization is
 * refused. And completing a session or reporting its usage is for
 * the session's **creator** or an admin token alone — not the
 * workspace's owner — because several agents share one workspace and
 * an owner check would let any of them close another's session.
 */
export interface SessionRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** The workspace a session names, and whether this caller may open one. */
async function openableWorkspace(c: Context, config: SessionRouteConfig, slug: string) {
  const workspace = await findWorkspace(config.store, slug);
  if (workspace === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  // Write access, whatever the workspace's status: an agent working
  // on an archived workspace still spent what it spent.
  const level = await accessLevel(config.store, identityOf(c), workspace);
  if (level !== 'owner' && level !== 'admin') {
    throw new HttpError(404, 'workspace not found');
  }
  return workspace;
}

/** Whether this caller may read the session's workspace at all. */
async function mayReadWorkspace(
  config: SessionRouteConfig,
  identity: Identity | undefined,
  slug: string,
): Promise<boolean> {
  const workspace = await findWorkspace(config.store, slug);
  if (workspace === undefined) {
    return false;
  }
  return (await accessLevel(config.store, identity, workspace)) !== undefined;
}

async function openSession(c: Context, config: SessionRouteConfig): Promise<Response> {
  const raw: unknown = await c.req.json().catch(() => undefined);
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const slug = body['workspace_slug'];
  if (typeof slug !== 'string' || slug.length === 0) {
    throw new HttpError(400, 'workspace_slug is required');
  }

  const identity = identityOf(c);
  const asked = typeof body['id'] === 'string' && body['id'].length > 0 ? body['id'] : undefined;

  // An open that repeats an id is the same open, not a second one:
  // an agent that retries a request it never saw answered should not
  // end up with two sessions.
  if (asked !== undefined) {
    const held = await sessionById(config.store, asked);
    if (held !== undefined) {
      if (!(await mayReadWorkspace(config, identity, held.workspace_slug))) {
        throw new HttpError(404, 'workspace not found');
      }
      return jsonResponse(serialiseSession(held));
    }
  }

  const workspace = await openableWorkspace(c, config, slug);
  const id = asked ?? newIdentifier();
  const text = (name: string) =>
    typeof body[name] === 'string' && body[name].length > 0 ? (body[name] as string) : undefined;
  const metadata = body['metadata'];

  await insertSession(
    config.store,
    {
      id,
      workspace_slug: workspace.slug,
      credential_type: identity?.kind ?? 'unknown',
      ...(identity?.credentialId === undefined ? {} : { credential_id: identity.credentialId }),
      ...(identity?.userId === undefined ? {} : { created_by: identity.userId }),
      ...(text('run_id') === undefined ? {} : { run_id: text('run_id') as string }),
      ...(text('node_id') === undefined ? {} : { node_id: text('node_id') as string }),
      ...(text('archetype') === undefined ? {} : { archetype: text('archetype') as string }),
      ...(text('model') === undefined ? {} : { model: text('model') as string }),
      ...(typeof metadata === 'object' && metadata !== null && !Array.isArray(metadata)
        ? { metadata: metadata as Record<string, unknown> }
        : {}),
    },
    config.clock.now().toISOString(),
  );

  return jsonResponse(
    serialiseSession((await sessionById(config.store, id)) as SessionRecord),
    201,
  );
}

export function sessionRoutes(config: SessionRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/sessions`,
      methods: ['POST'],
      scope: 'sessions:write',
      handler: async (c) => openSession(c, config),
    },
    {
      path: `${config.mountPoint}/sessions/:id/complete`,
      methods: ['POST'],
      scope: 'sessions:write',
      handler: async (c) => {
        const asked = readCompleteBody(await c.req.json().catch(() => undefined));
        const session = await ownedSession(config.store, c.req.param('id') ?? '', identityOf(c));
        return jsonResponse(
          serialiseSession(await completeSession(config.store, config.clock, session, asked)),
        );
      },
    },
    {
      path: `${config.mountPoint}/sessions/:id/usage`,
      methods: ['POST'],
      scope: 'sessions:write',
      handler: async (c) => {
        const report = readUsageBody(await c.req.json().catch(() => undefined));
        const session = await ownedSession(config.store, c.req.param('id') ?? '', identityOf(c));
        return jsonResponse(await recordUsage(config.store, config.clock, session, report), 201);
      },
    },
  ];
}

export { tokenSummary };
