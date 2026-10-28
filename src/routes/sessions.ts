import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { newIdentifier } from '../auth/credentials.js';
import { type Identity, identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { page, readPageRequest } from '../paging/page.js';
import { completeSession, ownedSession, readCompleteBody } from '../session/complete.js';
import { DENIED, listSessions, listUsage, mayReadSessions } from '../session/read.js';
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

/**
 * The session a read names, refused where its workspace is archived
 * or out of reach. That refusal is a 403 rather than a 404: this is
 * the one family of routes where the reference prefers saying the
 * workspace is inactive to hiding that it exists.
 */
async function readableSession(c: Context, config: SessionRouteConfig): Promise<SessionRecord> {
  const session = await sessionById(config.store, c.req.param('id') ?? '');
  if (session === undefined) {
    throw new HttpError(404, 'session not found');
  }
  if (!(await mayReadSessions(config.store, identityOf(c), session.workspace_slug))) {
    throw new HttpError(403, DENIED);
  }
  return session;
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
      path: `${config.mountPoint}/sessions`,
      scope: 'sessions:read',
      handler: async (c) => {
        const query = new URL(c.req.url).searchParams;
        const request = readPageRequest(query, { fallback: 50, most: 500 });
        const named = (name: string) => query.get(name) ?? undefined;

        const found = await listSessions(
          config.store,
          identityOf(c),
          {
            ...(named('workspace_slug') === undefined
              ? {}
              : { workspace_slug: named('workspace_slug') as string }),
            ...(named('run_id') === undefined ? {} : { run_id: named('run_id') as string }),
            ...(named('status') === undefined ? {} : { status: named('status') as string }),
            ...(named('credential_type') === undefined
              ? {}
              : { credential_type: named('credential_type') as string }),
            ...(named('since') === undefined ? {} : { since: named('since') as string }),
          },
          request,
        );
        const served = page(found, request, (one) => ({ at: one.started_at, id: one.id }));

        return jsonResponse({
          sessions: await Promise.all(
            served.rows.map(async (one) =>
              serialiseSession(one, {
                token_summary: await tokenSummary(config.store, one.id),
              }),
            ),
          ),
          next_cursor: served.next_cursor,
          has_more: served.has_more,
        });
      },
    },
    {
      path: `${config.mountPoint}/sessions/:id`,
      scope: 'sessions:read',
      handler: async (c) => {
        const session = await readableSession(c, config);
        return jsonResponse(
          serialiseSession(session, {
            token_summary: await tokenSummary(config.store, session.id),
          }),
        );
      },
    },
    {
      path: `${config.mountPoint}/sessions/:id/usage`,
      scope: 'sessions:read',
      handler: async (c) => {
        const session = await readableSession(c, config);
        const request = readPageRequest(new URL(c.req.url).searchParams, {
          fallback: 100,
          most: 1000,
        });
        const found = await listUsage(config.store, session.id, request);
        const served = page(found, request, (one) => ({ at: one.recorded_at, id: one.id }));

        return jsonResponse({
          session_id: session.id,
          records: served.rows,
          // Over every record of the session, whatever the page: a
          // total of one page is not a total.
          totals: await tokenSummary(config.store, session.id),
          next_cursor: served.next_cursor,
          has_more: served.has_more,
        });
      },
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
