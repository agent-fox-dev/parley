import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import type { Identity } from '../auth/middleware.js';
import { afterClause, type PageRequest } from '../paging/page.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { type SessionRecord, type UsageRecord, toSession, toUsage } from './store.js';

/**
 * Reading sessions and what they spent.
 *
 * These reads need read access **and** an active workspace, which is
 * the one family of routes where the reference prefers telling the
 * caller the workspace is inactive over hiding that it exists: a
 * member of an archived workspace is told `workspace access denied`
 * rather than that nothing is there.
 */
export const DENIED = 'workspace access denied';

/** Whether this caller may read the sessions of this workspace. */
export async function mayReadSessions(
  store: RelationalStore,
  identity: Identity | undefined,
  slug: string,
): Promise<boolean> {
  if (identity?.kind === 'admin_token') {
    return true;
  }
  const workspace = await findWorkspace(store, slug);
  if (workspace === undefined || workspace.status === 'archived') {
    return false;
  }
  return (await accessLevel(store, identity, workspace)) !== undefined;
}

/** The workspaces whose sessions this caller may see at all. */
async function readableWorkspaces(
  store: RelationalStore,
  identity: Identity | undefined,
): Promise<string[] | 'every'> {
  if (identity?.kind === 'admin_token') {
    return 'every';
  }
  const rows = await store.all<{ slug: string }>(
    "SELECT slug FROM workspaces WHERE status != 'archived'",
    [],
  );
  const readable: string[] = [];
  for (const row of rows) {
    if (await mayReadSessions(store, identity, row.slug)) {
      readable.push(row.slug);
    }
  }
  return readable;
}

export interface SessionFilters {
  workspace_slug?: string;
  run_id?: string;
  status?: string;
  credential_type?: string;
  since?: string;
}

/**
 * One page of sessions, narrowed to what this caller may see. A
 * caller with no readable workspace is answered an empty page rather
 * than a refusal: nothing to show is not an error.
 */
export async function listSessions(
  store: RelationalStore,
  identity: Identity | undefined,
  filters: SessionFilters,
  request: PageRequest,
): Promise<SessionRecord[]> {
  if (filters.workspace_slug !== undefined) {
    if (!(await mayReadSessions(store, identity, filters.workspace_slug))) {
      throw new HttpError(403, DENIED);
    }
  }

  const readable = await readableWorkspaces(store, identity);
  if (readable !== 'every' && readable.length === 0) {
    return [];
  }

  const where: string[] = ['1 = 1'];
  const params: unknown[] = [];
  if (readable !== 'every') {
    where.push(`workspace_slug IN (${readable.map(() => '?').join(', ')})`);
    params.push(...readable);
  }
  for (const [column, value] of [
    ['workspace_slug', filters.workspace_slug],
    ['run_id', filters.run_id],
    ['status', filters.status],
    ['credential_type', filters.credential_type],
  ] as const) {
    if (value !== undefined) {
      where.push(`${column} = ?`);
      params.push(value);
    }
  }
  if (filters.since !== undefined) {
    where.push('started_at > ?');
    params.push(filters.since);
  }

  const after = afterClause(request, { at: 'started_at', id: 'id' });
  const rows = await store.all(
    `SELECT * FROM sessions WHERE ${where.join(' AND ')}${after.sql}
     ORDER BY started_at ${request.order === 'asc' ? 'ASC' : 'DESC'},
              id ${request.order === 'asc' ? 'ASC' : 'DESC'}
     LIMIT ?`,
    [...params, ...after.params, request.limit + 1],
  );
  return rows.map((row) => toSession(row));
}

/** One page of a session's usage records. */
export async function listUsage(
  store: RelationalStore,
  sessionId: string,
  request: PageRequest,
): Promise<UsageRecord[]> {
  const after = afterClause(request, { at: 'recorded_at', id: 'id' });
  const rows = await store.all(
    `SELECT * FROM session_usage WHERE session_id = ?${after.sql}
     ORDER BY recorded_at ${request.order === 'asc' ? 'ASC' : 'DESC'},
              id ${request.order === 'asc' ? 'ASC' : 'DESC'}
     LIMIT ?`,
    [sessionId, ...after.params, request.limit + 1],
  );
  return rows.map((row) => toUsage(row));
}
