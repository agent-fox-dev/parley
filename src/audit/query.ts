import { HttpError } from '../http/errors.js';
import type { Identity } from '../auth/middleware.js';
import type { RelationalStore, Row } from '../platform/index.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { type PageRequest, afterClause } from '../paging/page.js';

/**
 * The one query over the two sources.
 *
 * Both are read as one list ordered by instant, which is the whole
 * difficulty: the service writes every timestamp through
 * `toISOString`, so the stored strings are all of one precision and
 * comparing them as text is comparing them as instants. A row
 * written any other way would sort by its spelling, which is why
 * nothing writes one any other way.
 *
 * A filter only hub events carry — an actor, a resource, an action —
 * leaves the agent events out rather than matching none of them, and
 * a run filter leaves the hub events out. That is the reference's
 * rule and not an accident of the SQL.
 */
export interface AuditFilters {
  source?: 'hub' | 'agent';
  workspace?: string;
  event_type?: string;
  event_type_prefix?: string;
  severity?: string;
  actor_id?: string;
  actor_type?: string;
  resource_type?: string;
  action?: string;
  run_id?: string;
  since?: string;
  until?: string;
}

/** The filters that only a hub event can answer. */
const HUB_ONLY = ['actor_id', 'actor_type', 'resource_type', 'action'] as const;

export interface AuditEvent extends Row {
  id: string;
  event_type: string;
  source: 'hub' | 'agent';
  timestamp: string;
  severity: string;
  workspace: string | null;
  actor_id: string | null;
  actor_type: string | null;
  resource_type: string | null;
  resource_id: string | null;
  action: string | null;
  run_id: string | null;
  node_id: string | null;
  session_id: string | null;
  archetype: string | null;
}

/** What the query string asks for, within what the route allows. */
export function readFilters(query: URLSearchParams): AuditFilters {
  const source = query.get('source');
  if (source !== null && source !== 'hub' && source !== 'agent') {
    throw new HttpError(400, 'source must be hub or agent');
  }

  const filters: AuditFilters = {};
  if (source !== null) {
    filters.source = source;
  }
  for (const name of [
    'workspace',
    'event_type',
    'event_type_prefix',
    'severity',
    'run_id',
    ...HUB_ONLY,
  ] as const) {
    const given = query.get(name);
    if (given !== null && given.length > 0) {
      filters[name] = given;
    }
  }

  for (const name of ['since', 'until'] as const) {
    const given = query.get(name);
    if (given === null || given.length === 0) {
      continue;
    }
    if (Number.isNaN(Date.parse(given))) {
      throw new HttpError(400, `${name} must be an RFC 3339 time`);
    }
    filters[name] = new Date(given).toISOString();
  }
  if (
    filters.since !== undefined &&
    filters.until !== undefined &&
    Date.parse(filters.since) >= Date.parse(filters.until)
  ) {
    throw new HttpError(400, 'since must be before until');
  }

  return filters;
}

/**
 * The workspaces whose events this caller may be shown: every one
 * they own or share an organization with, whatever its status. An
 * admin token sees them all, and the events of no workspace besides.
 */
export async function visibleWorkspaces(
  store: RelationalStore,
  identity: Identity | undefined,
): Promise<string[] | 'every'> {
  if (identity?.kind === 'admin_token') {
    return 'every';
  }
  const rows = await store.all<{ slug: string }>('SELECT slug FROM workspaces', []);
  const visible: string[] = [];
  for (const row of rows) {
    const workspace = await findWorkspace(store, row.slug);
    if (workspace === undefined) {
      continue;
    }
    if ((await accessLevel(store, identity, workspace)) !== undefined) {
      visible.push(workspace.slug);
    }
  }
  return visible;
}

/** Whether one source is left out by the filters the caller gave. */
function wanted(filters: AuditFilters): { hub: boolean; agent: boolean } {
  const hubOnly = HUB_ONLY.some((name) => filters[name] !== undefined);
  const agentOnly = filters.run_id !== undefined;
  return {
    hub: filters.source !== 'agent' && !agentOnly,
    agent: filters.source !== 'hub' && !hubOnly,
  };
}

function common(filters: AuditFilters, column: { type: string; at: string }) {
  const where: string[] = [];
  const params: unknown[] = [];
  if (filters.event_type !== undefined) {
    where.push(`${column.type} = ?`);
    params.push(filters.event_type);
  }
  if (filters.event_type_prefix !== undefined) {
    where.push(`${column.type} LIKE ?`);
    params.push(`${filters.event_type_prefix}%`);
  }
  if (filters.since !== undefined) {
    where.push(`${column.at} >= ?`);
    params.push(filters.since);
  }
  if (filters.until !== undefined) {
    where.push(`${column.at} < ?`);
    params.push(filters.until);
  }
  return { where, params };
}

function places(values: readonly string[]): string {
  return values.map(() => '?').join(', ');
}

/**
 * One page of the two sources together. The rows are selected with
 * the same columns in the same order from both, so the union is one
 * list the database orders rather than two the service merges.
 */
/** What one source contributes to the union, and how it is narrowed. */
interface Half {
  /** The columns, in the order both halves must agree on. */
  select: string;
  table: string;
  /** The column holding the workspace, which the two tables spell differently. */
  workspace: string;
  /** The filters only this source answers. */
  own: readonly string[];
}

const HUB: Half = {
  select: `id, event_type, 'hub' AS source, timestamp, severity, workspace,
              actor_id, actor_type, resource_type, resource_id, action,
              NULL AS run_id, NULL AS node_id, NULL AS session_id`,
  table: 'hub_events',
  workspace: 'workspace',
  own: HUB_ONLY,
};

const AGENT: Half = {
  select: `id, event_type, 'agent' AS source, timestamp, severity, workspace_slug AS workspace,
              NULL AS actor_id, NULL AS actor_type, NULL AS resource_type, NULL AS resource_id,
              NULL AS action, run_id, node_id, session_id`,
  table: 'run_events',
  workspace: 'workspace_slug',
  own: ['run_id'],
};

/** One half of the union: its columns, its narrowing and its values. */
function half(
  source: Half,
  visible: string[] | 'every',
  filters: AuditFilters,
  after: { sql: string; params: unknown[] },
): { sql: string; params: unknown[] } {
  const { where, params } = common(filters, { type: 'event_type', at: 'timestamp' });

  for (const name of [...source.own, 'severity'] as const) {
    const value = filters[name as keyof AuditFilters];
    if (value !== undefined) {
      where.push(`${name} = ?`);
      params.push(value);
    }
  }

  if (filters.workspace !== undefined) {
    where.push(`${source.workspace} = ?`);
    params.push(filters.workspace);
  } else if (visible !== 'every') {
    // An event of no workspace belongs to the deployment, and only an
    // admin token is shown one.
    where.push(visible.length === 0 ? '1 = 0' : `${source.workspace} IN (${places(visible)})`);
    params.push(...visible);
  }

  return {
    sql: `SELECT ${source.select}
         FROM ${source.table}
        WHERE ${[...where, '1 = 1'].join(' AND ')}${after.sql}`,
    params: [...params, ...after.params],
  };
}

/**
 * One page of the two sources together. Both halves are selected with
 * the same columns in the same order, so the union is one list the
 * database orders rather than two the service merges — which is what
 * makes paging to the end yield every event exactly once.
 */
export async function queryEvents(
  store: RelationalStore,
  visible: string[] | 'every',
  filters: AuditFilters,
  request: PageRequest,
): Promise<AuditEvent[]> {
  const take = wanted(filters);
  const after = afterClause(request, { at: 'timestamp', id: 'id' });
  const halves = [
    ...(take.hub ? [half(HUB, visible, filters, after)] : []),
    ...(take.agent ? [half(AGENT, visible, filters, after)] : []),
  ];

  if (halves.length === 0) {
    return [];
  }

  const direction = request.order === 'asc' ? 'ASC' : 'DESC';
  const rows = await store.all<Row>(
    `${halves.map((one) => one.sql).join(' UNION ALL ')}
      ORDER BY timestamp ${direction}, id ${direction}
      LIMIT ?`,
    [...halves.flatMap((one) => one.params), request.limit + 1],
  );

  return rows.map((row) => ({ ...row, archetype: null }) as AuditEvent);
}
