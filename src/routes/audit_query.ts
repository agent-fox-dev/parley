import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { page, readPageRequest } from '../paging/page.js';
import { queryEvents, readFilters, visibleWorkspaces } from '../audit/query.js';

/**
 * `GET /audit`: hub events and agent events as one timeline. A
 * workspace the caller may not read is answered as one that is not
 * there, so the query tells nobody which workspaces exist.
 */
export interface AuditQueryConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

async function query(c: Context, config: AuditQueryConfig): Promise<Response> {
  const asked = new URL(c.req.url).searchParams;
  const filters = readFilters(asked);
  const request = readPageRequest(asked, { fallback: 100, most: 1000 });
  const identity = identityOf(c);

  if (filters.workspace !== undefined) {
    const workspace = await findWorkspace(config.store, filters.workspace);
    if (
      workspace === undefined ||
      (await accessLevel(config.store, identity, workspace)) === undefined
    ) {
      throw new HttpError(404, 'workspace not found');
    }
  }

  const visible = await visibleWorkspaces(config.store, identity);
  const found = await queryEvents(config.store, visible, filters, request);
  const served = page(found, request, (row) => ({ at: row.timestamp, id: row.id }));

  return jsonResponse({
    events: served.rows,
    next_cursor: served.next_cursor,
    has_more: served.has_more,
  });
}

export function auditQueryRoutes(config: AuditQueryConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/audit`,
      scope: 'audit:read',
      handler: async (c) => query(c, config),
    },
  ];
}
