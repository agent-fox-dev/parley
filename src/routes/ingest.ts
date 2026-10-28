import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import {
  type RecordKind,
  checkedRunId,
  identityOfRecord,
  readRecord,
  storeRecord,
} from '../audit/run_records.js';

/**
 * What an agent reports about a run.
 *
 * The access rule reads backwards compared with the rest of the
 * service and is written out here in full because of it: the
 * workspace must exist (404), then it must not be archived (409,
 * answered *before* anybody's access is considered), and then the
 * caller must own it (403, not the 404 the anti-enumeration routes
 * would answer). A member of the owning organization may read this
 * run's records and may not write one.
 */
export interface IngestRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** How many items one batch may carry. */
const MOST_IN_A_BATCH = 1000;

const ARCHIVED = 'workspace_archived';
const DENIED = 'workspace_access_denied';

/** The workspace an ingestion names, in the order the reference checks. */
export async function ingestableWorkspace(c: Context, config: IngestRouteConfig): Promise<string> {
  const slug = c.req.param('slug') ?? '';
  const workspace = await findWorkspace(config.store, slug);
  if (workspace === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  if (workspace.status === 'archived') {
    throw new HttpError(409, 'this workspace is archived', ARCHIVED);
  }
  const level = await accessLevel(config.store, identityOf(c), workspace);
  if (level !== 'owner' && level !== 'admin') {
    throw new HttpError(403, 'token owner does not have write access to this workspace', DENIED);
  }
  return workspace.slug;
}

async function ingestOne(
  c: Context,
  config: IngestRouteConfig,
  kind: RecordKind,
): Promise<Response> {
  const slug = await ingestableWorkspace(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');
  const record = readRecord(kind, await c.req.json().catch(() => undefined), {
    slug,
    runId,
    now: config.clock.now().toISOString(),
  });

  const outcome = await storeRecord(config.store, kind, record);
  return jsonResponse(
    await identityOfRecord(config.store, kind, record.id),
    outcome === 'stored' ? 201 : 200,
  );
}

async function ingestBatch(
  c: Context,
  config: IngestRouteConfig,
  kind: RecordKind,
): Promise<Response> {
  const slug = await ingestableWorkspace(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');

  const raw: unknown = await c.req.json().catch(() => undefined);
  if (!Array.isArray(raw)) {
    throw new HttpError(400, 'a batch must be a JSON array');
  }
  if (raw.length === 0) {
    throw new HttpError(400, 'a batch must hold at least one item');
  }
  if (raw.length > MOST_IN_A_BATCH) {
    throw new HttpError(413, `a batch holds at most ${String(MOST_IN_A_BATCH)} items`);
  }

  const now = config.clock.now().toISOString();
  const errors: Array<{ index: number; id?: string; message: string }> = [];
  let accepted = 0;
  let duplicates = 0;

  // Each item stands on its own: one that cannot be read is reported
  // by its index and the rest are stored anyway, because an agent
  // that sent ninety-nine good records should not lose them to one.
  for (const [index, item] of raw.entries()) {
    try {
      const record = readRecord(kind, item, { slug, runId, now });
      const outcome = await storeRecord(config.store, kind, record);
      if (outcome === 'stored') {
        accepted += 1;
      } else {
        duplicates += 1;
      }
    } catch (err) {
      const id = (item as Record<string, unknown> | null)?.['id'];
      errors.push({
        index,
        ...(typeof id === 'string' ? { id } : {}),
        message: err instanceof Error ? err.message : 'the item could not be read',
      });
    }
  }

  return jsonResponse({ accepted, duplicates, errors });
}

/** The path each kind is ingested at, under the run. */
const SINGLE: ReadonlyArray<{ path: string; kind: RecordKind }> = [
  { path: 'events', kind: 'events' },
  { path: 'sessions/outcomes', kind: 'outcomes' },
  { path: 'tools/calls', kind: 'calls' },
  { path: 'tools/errors', kind: 'errors' },
  { path: 'traces', kind: 'traces' },
];

/** The two kinds that may be sent in bulk. */
const BATCHED: ReadonlyArray<{ path: string; kind: RecordKind }> = [
  { path: 'events/batch', kind: 'events' },
  { path: 'traces/batch', kind: 'traces' },
];

export function ingestRoutes(config: IngestRouteConfig): RouteDefinition[] {
  const under = (path: string) => `${config.mountPoint}/workspaces/:slug/runs/:run_id/${path}`;

  return [
    ...SINGLE.map(({ path, kind }) => ({
      path: under(path),
      methods: ['POST'],
      scope: 'audit:write',
      handler: async (c: Context) => ingestOne(c, config, kind),
    })),
    ...BATCHED.map(({ path, kind }) => ({
      path: under(path),
      methods: ['POST'],
      scope: 'audit:write',
      handler: async (c: Context) => ingestBatch(c, config, kind),
    })),
  ];
}
