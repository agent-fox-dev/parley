import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { checkedRunId } from '../audit/run_records.js';
import { type IngestRouteConfig, ingestableWorkspace } from './ingest.js';

/**
 * The account of how a run ended. A run has at most one, and the one
 * it has is the first submitted: an agent that retries the submit
 * gets its own first account back rather than overwriting it with the
 * retry's, which is the whole reason this is not an upsert.
 */
export type PostmortemRouteConfig = IngestRouteConfig;

/** The ways a run can have ended, as the reference names them. */
const RUN_STATUSES: readonly string[] = ['stalled', 'block_limit', 'cost_limit', 'session_limit'];

/** The only schema this service knows how to read. */
const SCHEMA_VERSION = 1;

interface StoredPostmortem {
  [column: string]: unknown;
  run_id: string;
  workspace_slug: string;
  schema_version: number;
  run_status: string;
  started_at: string;
  completed_at: string;
  task_summary: string;
  cost_summary: string;
  blocked_tasks: string;
  session_history: string;
  ingested_at: string;
}

function object(body: Record<string, unknown>, name: string): Record<string, unknown> {
  const value = body[name];
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new HttpError(400, `${name} is required and must be a JSON object`);
  }
  return value as Record<string, unknown>;
}

function time(body: Record<string, unknown>, name: string): string {
  const value = body[name];
  if (typeof value !== 'string' || value.length === 0) {
    throw new HttpError(400, `${name} is required`);
  }
  if (Number.isNaN(Date.parse(value))) {
    throw new HttpError(400, `${name} must be an RFC 3339 time`);
  }
  return new Date(value).toISOString();
}

function list(body: Record<string, unknown>, name: string): unknown[] {
  const value = body[name];
  if (value === undefined || value === null) {
    return [];
  }
  if (!Array.isArray(value)) {
    throw new HttpError(400, `${name} must be an array`);
  }
  return value;
}

/** The postmortem a request carries, checked in the reference's order. */
export function readPostmortem(
  raw: unknown,
): Omit<StoredPostmortem, 'workspace_slug' | 'run_id' | 'ingested_at'> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const version = body['schema_version'];
  if (version !== undefined && version !== null && version !== SCHEMA_VERSION) {
    throw new HttpError(
      422,
      `this service reads schema version ${String(SCHEMA_VERSION)} alone`,
      'unknown_schema_version',
    );
  }

  const status = body['run_status'];
  if (typeof status !== 'string' || !RUN_STATUSES.includes(status)) {
    throw new HttpError(400, `run_status must be one of ${RUN_STATUSES.join(', ')}`);
  }

  const tasks = object(body, 'task_summary');
  if (tasks['total'] === undefined) {
    throw new HttpError(400, 'task_summary must carry a total');
  }

  return {
    schema_version: SCHEMA_VERSION,
    run_status: status,
    started_at: time(body, 'started_at'),
    completed_at: time(body, 'completed_at'),
    task_summary: JSON.stringify(tasks),
    cost_summary: JSON.stringify(object(body, 'cost_summary')),
    blocked_tasks: JSON.stringify(list(body, 'blocked_tasks')),
    session_history: JSON.stringify(list(body, 'session_history')),
  };
}

async function heldPostmortem(
  store: RelationalStore,
  slug: string,
  runId: string,
): Promise<StoredPostmortem | undefined> {
  const rows = await store.all<StoredPostmortem>(
    'SELECT * FROM run_postmortems WHERE workspace_slug = ? AND run_id = ?',
    [slug, runId],
  );
  return rows[0];
}

/** The postmortem as a client reads it, with its JSON fields unpacked. */
function serialise(held: StoredPostmortem): Record<string, unknown> {
  return {
    run_id: held.run_id,
    workspace: held.workspace_slug,
    schema_version: held.schema_version,
    run_status: held.run_status,
    started_at: held.started_at,
    completed_at: held.completed_at,
    task_summary: JSON.parse(held.task_summary),
    cost_summary: JSON.parse(held.cost_summary),
    blocked_tasks: JSON.parse(held.blocked_tasks),
    session_history: JSON.parse(held.session_history),
    ingested_at: held.ingested_at,
  };
}

async function submit(c: Context, config: PostmortemRouteConfig): Promise<Response> {
  const slug = await ingestableWorkspace(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');
  const asked = readPostmortem(await c.req.json().catch(() => undefined));

  const held = await heldPostmortem(config.store, slug, runId);
  if (held !== undefined) {
    return jsonResponse({
      run_id: held.run_id,
      run_status: held.run_status,
      created_at: held.ingested_at,
    });
  }

  const at = config.clock.now().toISOString();
  await config.store.run(
    `INSERT INTO run_postmortems
       (run_id, workspace_slug, schema_version, run_status, started_at, completed_at,
        task_summary, cost_summary, blocked_tasks, session_history, ingested_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      runId,
      slug,
      asked.schema_version,
      asked.run_status,
      asked.started_at,
      asked.completed_at,
      asked.task_summary,
      asked.cost_summary,
      asked.blocked_tasks,
      asked.session_history,
      at,
    ],
  );

  return jsonResponse({ run_id: runId, run_status: asked.run_status, created_at: at }, 201);
}

/**
 * A read is open to anyone who may read the workspace, whatever its
 * status — the opposite of the write, which is the owner's alone.
 */
export async function readableRun(c: Context, config: PostmortemRouteConfig): Promise<string> {
  const slug = c.req.param('slug') ?? '';
  const workspace = await findWorkspace(config.store, slug);
  if (workspace === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  if ((await accessLevel(config.store, identityOf(c), workspace)) === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  return workspace.slug;
}

async function read(c: Context, config: PostmortemRouteConfig): Promise<Response> {
  const slug = await readableRun(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');

  const held = await heldPostmortem(config.store, slug, runId);
  if (held === undefined) {
    throw new HttpError(404, 'this run has no postmortem', 'postmortem_not_found');
  }
  return jsonResponse(serialise(held));
}

export function postmortemRoutes(config: PostmortemRouteConfig): RouteDefinition[] {
  const path = `${config.mountPoint}/workspaces/:slug/runs/:run_id/postmortem`;
  return [
    {
      path,
      methods: ['POST'],
      scope: 'audit:write',
      handler: async (c) => submit(c, config),
    },
    {
      path,
      scope: 'audit:read',
      handler: async (c) => read(c, config),
    },
  ];
}
