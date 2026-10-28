import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Row } from '../platform/index.js';
import { afterClause, page, readPageRequest } from '../paging/page.js';
import { type RecordKind, RECORD_SHAPES, checkedRunId } from '../audit/run_records.js';
import { type PostmortemRouteConfig, readableRun } from './postmortem.js';

/**
 * Reading back what an agent reported.
 *
 * The five lists differ only in the table they read, the name they
 * answer with and the columns a caller may filter on, so that is all
 * that is written per kind. Everything else — the order, the page
 * size, the cursor, the time bounds — is the pagination facility, the
 * same one the session lists use.
 */
export type RunReadConfig = PostmortemRouteConfig;

interface ListShape {
  kind: RecordKind;
  /** The path under the run, and the name of the collection answered. */
  path: string;
  named: string;
  /** The columns a caller may narrow by, each matched exactly. */
  filters: readonly string[];
}

const LISTS: readonly ListShape[] = [
  {
    kind: 'events',
    path: 'events',
    named: 'events',
    filters: ['event_type', 'severity', 'node_id', 'session_id'],
  },
  {
    kind: 'outcomes',
    path: 'sessions/outcomes',
    named: 'outcomes',
    filters: ['session_id', 'status', 'node_id'],
  },
  {
    kind: 'calls',
    path: 'tools/calls',
    named: 'calls',
    filters: ['tool_name', 'session_id', 'node_id'],
  },
  {
    kind: 'errors',
    path: 'tools/errors',
    named: 'errors',
    filters: ['tool_name', 'session_id', 'node_id'],
  },
  {
    kind: 'traces',
    path: 'traces',
    named: 'traces',
    filters: ['event_type', 'session_id', 'node_id'],
  },
];

/** The lower and upper bounds a read may ask for, checked as a pair. */
export function readPeriod(query: URLSearchParams): { since?: string; until?: string } {
  const bounds: { since?: string; until?: string } = {};
  for (const name of ['since', 'until'] as const) {
    const given = query.get(name);
    if (given === null || given.length === 0) {
      continue;
    }
    if (Number.isNaN(Date.parse(given))) {
      throw new HttpError(400, `${name} must be an RFC 3339 time`);
    }
    bounds[name] = new Date(given).toISOString();
  }
  if (
    bounds.since !== undefined &&
    bounds.until !== undefined &&
    Date.parse(bounds.since) >= Date.parse(bounds.until)
  ) {
    throw new HttpError(400, 'since must be before until');
  }
  return bounds;
}

/** The record as a client reads it, with the JSON columns unpacked. */
function serialise(kind: RecordKind, row: Row): Record<string, unknown> {
  const shape = RECORD_SHAPES[kind];
  const read: Record<string, unknown> = { ...row };
  for (const column of shape.json) {
    const held = row[column];
    read[column] = typeof held === 'string' ? JSON.parse(held) : null;
  }
  return read;
}

async function list(c: Context, config: RunReadConfig, shape: ListShape): Promise<Response> {
  const slug = await readableRun(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');

  const query = new URL(c.req.url).searchParams;
  const request = readPageRequest(query, { fallback: 50, most: 500 });
  const period = readPeriod(query);

  const table = RECORD_SHAPES[shape.kind].table;
  const at = RECORD_SHAPES[shape.kind].time;

  const where = ['workspace_slug = ?', 'run_id = ?'];
  const params: unknown[] = [slug, runId];
  for (const filter of shape.filters) {
    const given = query.get(filter);
    if (given !== null && given.length > 0) {
      where.push(`${filter} = ?`);
      params.push(given);
    }
  }
  // `since` takes the record recorded at that instant; `until` does not.
  if (period.since !== undefined) {
    where.push(`${at} >= ?`);
    params.push(period.since);
  }
  if (period.until !== undefined) {
    where.push(`${at} < ?`);
    params.push(period.until);
  }

  const after = afterClause(request, { at, id: 'id' });
  const direction = request.order === 'asc' ? 'ASC' : 'DESC';
  const rows = await config.store.all<Row>(
    `SELECT * FROM ${table}
      WHERE ${where.join(' AND ')}${after.sql}
      ORDER BY ${at} ${direction}, id ${direction}
      LIMIT ?`,
    [...params, ...after.params, request.limit + 1],
  );

  const served = page(rows, request, (row) => ({
    at: String(row[at]),
    id: String(row['id']),
  }));

  return jsonResponse({
    [shape.named]: served.rows.map((row) => serialise(shape.kind, row)),
    next_cursor: served.next_cursor,
    has_more: served.has_more,
  });
}

export function runReadRoutes(config: RunReadConfig): RouteDefinition[] {
  return LISTS.map((shape) => ({
    path: `${config.mountPoint}/workspaces/:slug/runs/:run_id/${shape.path}`,
    scope: 'audit:read',
    handler: async (c: Context) => list(c, config, shape),
  }));
}
