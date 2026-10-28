import { type EventRecorder, actorIn, record } from '../audit/recorder.js';
import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { newIdentifier } from '../auth/credentials.js';
import { identityOf } from '../auth/middleware.js';
import { queueMerge } from '../jobs/merge.js';
import { branchNameRefusal } from '../push/checks.js';
import { repositoryOf } from '../repo/repository.js';
import { workspaceAccess } from '../workspace/access.js';
import {
  type MergeRecord,
  duplicateMerge,
  insertMerge,
  mergeById,
  mergesOf,
  serialiseMerge,
  updateMerge,
} from '../merge/job.js';

/**
 * The merge routes.
 *
 * The body is checked before the workspace is looked up, which is the
 * opposite of the anti-enumeration rule everywhere else and is what
 * the reference asks for: a malformed branch name is the caller's
 * mistake whatever workspace it names.
 */
export interface MergeRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  repos?: RepositoryStore;
  jobs?: JobQueue;
  /** Where this group writes down what it did. */
  events?: EventRecorder;
}

/** Both names a submit must carry, checked for syntax alone. */
export function readMergeBody(raw: unknown): { target_branch: string; source_ref: string } {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const asked: Record<string, string> = {};
  for (const name of ['target_branch', 'source_ref'] as const) {
    const value = body[name];
    if (typeof value !== 'string' || value.length === 0) {
      throw new HttpError(400, `${name} is required`);
    }
    const invalid = branchNameRefusal(value);
    if (invalid !== undefined) {
      throw new HttpError(400, `invalid ${name}: ${invalid.message}`);
    }
    asked[name] = value;
  }
  return { target_branch: asked['target_branch'] ?? '', source_ref: asked['source_ref'] ?? '' };
}

async function submit(c: Context, config: MergeRouteConfig): Promise<Response> {
  // The body first, as the reference states for this route.
  const asked = readMergeBody(await c.req.json().catch(() => undefined));

  const { workspace } = await workspaceAccess(c, config.store, 'write');
  if (workspace.status !== 'ready') {
    throw new HttpError(400, 'this workspace is not ready to merge');
  }
  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }

  const refs = await config.repos.listRefs(repositoryOf(workspace));
  for (const [name, branch] of [
    ['target_branch', asked.target_branch],
    ['source_ref', asked.source_ref],
  ] as const) {
    if (!refs.some((ref) => ref.name === `refs/heads/${branch}`)) {
      throw new HttpError(400, `${name} "${branch}" does not exist in this workspace`);
    }
  }

  const held = await duplicateMerge(
    config.store,
    workspace.slug,
    asked.target_branch,
    asked.source_ref,
  );
  if (held !== undefined) {
    throw new HttpError(
      409,
      `a merge of ${asked.source_ref} into ${asked.target_branch} is already under way`,
      'duplicate_merge',
    );
  }

  const id = newIdentifier();
  await insertMerge(
    config.store,
    {
      id,
      workspace_slug: workspace.slug,
      target_branch: asked.target_branch,
      source_ref: asked.source_ref,
      submitted_by: identityOf(c)?.userId ?? 'unknown',
    },
    config.clock.now().toISOString(),
  );
  if (config.jobs !== undefined) {
    await queueMerge(config.jobs, id);
  }

  await record(config.events, {
    type: 'hub.merge.enqueue',
    workspace: workspace.slug,
    resourceId: id,
    actor: actorIn(c),
    metadata: {
      target_branch: asked.target_branch,
      source_ref: asked.source_ref,
      job_id: id,
    },
  });

  return jsonResponse(serialiseMerge((await mergeById(config.store, id)) as MergeRecord), 202);
}

/** The job this route names, which must belong to this workspace. */
async function namedMerge(c: Context, config: MergeRouteConfig, need: 'read' | 'write') {
  const { workspace } = await workspaceAccess(c, config.store, need);
  const found = await mergeById(config.store, c.req.param('id') ?? '');
  if (found === undefined || found.workspace_slug !== workspace.slug) {
    throw new HttpError(404, 'merge not found');
  }
  return { workspace, job: found };
}

export function mergeRoutes(config: MergeRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/merges`,
      methods: ['POST'],
      scope: 'merges:write',
      handler: async (c) => submit(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/merges`,
      scope: 'merges:read',
      handler: async (c) => {
        const { workspace } = await workspaceAccess(c, config.store, 'read');
        const jobs = await mergesOf(config.store, workspace.slug);
        return jsonResponse(jobs.map((one) => serialiseMerge(one)));
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/merges/:id`,
      scope: 'merges:read',
      handler: async (c) => jsonResponse(serialiseMerge((await namedMerge(c, config, 'read')).job)),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/merges/:id`,
      methods: ['DELETE'],
      scope: 'merges:write',
      handler: async (c) => {
        const { job } = await namedMerge(c, config, 'write');
        if (job.status !== 'queued') {
          throw new HttpError(409, `a ${job.status} merge cannot be cancelled`);
        }
        await updateMerge(
          config.store,
          job.id,
          { status: 'cancelled' },
          config.clock.now().toISOString(),
        );
        return jsonResponse({ status: 'cancelled' });
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/merges/:id/requeue`,
      methods: ['POST'],
      scope: 'merges:write',
      handler: async (c) => {
        const { workspace, job } = await namedMerge(c, config, 'write');
        if (job.status !== 'dead_letter') {
          throw new HttpError(409, `a ${job.status} merge cannot be queued again`);
        }
        const held = await duplicateMerge(
          config.store,
          workspace.slug,
          job.target_branch,
          job.source_ref,
        );
        if (held !== undefined) {
          throw new HttpError(
            409,
            `a merge of ${job.source_ref} into ${job.target_branch} is already under way`,
            'duplicate_merge',
          );
        }
        await updateMerge(
          config.store,
          job.id,
          { status: 'queued', error: null, retry_count: job.retry_count + 1 },
          config.clock.now().toISOString(),
        );
        if (config.jobs !== undefined) {
          await queueMerge(config.jobs, job.id);
        }
        return jsonResponse(serialiseMerge((await mergeById(config.store, job.id)) as MergeRecord));
      },
    },
  ];
}
