import { type EventRecorder, actorIn, record as auditEvent } from '../audit/recorder.js';
import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { newIdentifier } from '../auth/credentials.js';
import { identityOf } from '../auth/middleware.js';
import { queueRebuild } from '../jobs/rebuild.js';
import { livePatches } from '../patch/list.js';
import { repositoryOf } from '../repo/repository.js';
import { previewRebuild } from '../rebuild/preview.js';
import {
  type FailMode,
  type RebuildRecord,
  type RebuildStrategy,
  insertRebuild,
  pendingRebuild,
  rebuildById,
  rebuildsOf,
  updateRebuild,
} from '../rebuild/record.js';
import { workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import { effectiveSettings } from '../workspace/settings.js';

/**
 * The rebuild routes: queue one, watch it, call it off, queue it
 * again, undo it, or ask what one would do.
 *
 * A rebuild is answered as a queued job rather than performed in the
 * request, because the work is far longer than a request may take.
 */
export interface RebuildRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  repos?: RepositoryStore;
  jobs?: JobQueue;
  log?: LogSink;
  /** Where this group writes down what it did. */
  events?: EventRecorder;
}

/** The job as a client reads it. */
export function serialiseRebuild(record: RebuildRecord): Record<string, unknown> {
  return {
    id: record.id,
    status: record.status,
    strategy: record.strategy,
    ...(record.error === null ? {} : { error: record.error }),
    created_at: record.queued_at,
    completed_at: record.finished_at,
    patch_results: record.results.map((one) => ({
      patch_id: one.patch_id,
      branch_name: one.branch_name,
      position: one.position,
      status:
        one.outcome === 'applied' ? 'success' : one.outcome === 'skipped' ? 'skipped' : 'conflict',
      ...(one.reason === undefined ? {} : { skipped_reason: one.reason }),
      ...(one.conflict_files === undefined ? {} : { conflict_files: one.conflict_files }),
    })),
    ...(record.new_integration_head === null
      ? {}
      : { integration_head_sha: record.new_integration_head }),
    ...(record.previous_integration_head === null
      ? {}
      : { previous_integration_head_sha: record.previous_integration_head }),
    ...(record.upstream_head === null ? {} : { upstream_head_sha: record.upstream_head }),
    ...(record.integration_branch_pushed ? { integration_branch_pushed: true } : {}),
    submitted_by: record.submitted_by,
  };
}

const STRATEGIES: readonly RebuildStrategy[] = ['rebase', 'merge'];
const FAIL_MODES: readonly FailMode[] = ['fail_fast', 'continue'];

async function submit(c: Context, config: RebuildRouteConfig): Promise<Response> {
  const { workspace } = await workspaceAccess(c, config.store, 'write');

  if (workspace.workspace_mode !== 'carry_patch') {
    throw new HttpError(
      400,
      'only a carry-patch workspace has a stack to rebuild',
      'workspace_mode_mismatch',
    );
  }
  if (workspace.status !== 'ready') {
    throw new HttpError(400, 'this workspace is not ready to rebuild');
  }

  const raw: unknown = await c.req.json().catch(() => undefined);
  if (raw !== undefined && (typeof raw !== 'object' || raw === null || Array.isArray(raw))) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = (raw ?? {}) as Record<string, unknown>;
  const settings = effectiveSettings(workspace.settings);

  const strategy = (body['strategy'] ?? settings['rebuild_strategy']) as RebuildStrategy;
  if (!STRATEGIES.includes(strategy)) {
    throw new HttpError(400, `strategy must be one of ${STRATEGIES.join(', ')}`);
  }
  const failMode = (body['fail_mode'] ?? settings['rebuild_fail_mode']) as FailMode;
  if (!FAIL_MODES.includes(failMode)) {
    throw new HttpError(400, `fail_mode must be one of ${FAIL_MODES.join(', ')}`);
  }

  const patches = await livePatches(config.store, workspace.slug);
  if (!patches.some((patch) => patch.status === 'active' || patch.status === 'conflict')) {
    throw new HttpError(400, 'this workspace carries no patch to rebuild', 'no_active_patches');
  }
  if ((await pendingRebuild(config.store, workspace.slug)) !== undefined) {
    throw new HttpError(
      409,
      'a rebuild of this workspace is already under way',
      'concurrent_rebuild',
    );
  }

  const id = newIdentifier();
  await insertRebuild(
    config.store,
    {
      id,
      workspace_slug: workspace.slug,
      strategy,
      fail_mode: failMode,
      submitted_by: identityOf(c)?.userId ?? 'unknown',
    },
    config.clock.now().toISOString(),
  );
  if (config.jobs !== undefined) {
    await queueRebuild(config.jobs, id);
  }

  await auditEvent(config.events, {
    type: 'hub.rebuild.enqueue',
    workspace: workspace.slug,
    resourceId: id,
    actor: actorIn(c),
    metadata: { job_id: id, patch_count: patches.length },
  });

  return jsonResponse(
    {
      id,
      type: 'rebuild',
      status: 'queued',
      key: workspace.slug,
      group_key: `${workspace.slug}:${workspace.integration_branch ?? 'integration'}`,
      payload: { workspace_slug: workspace.slug, strategy },
    },
    202,
  );
}

/** The job this route names, which must belong to this workspace. */
async function namedJob(c: Context, config: RebuildRouteConfig, need: 'read' | 'write') {
  const { workspace } = await workspaceAccess(c, config.store, need);
  const found = await rebuildById(config.store, c.req.param('id') ?? '');
  if (found === undefined || found.workspace_slug !== workspace.slug) {
    // A job of another workspace is one this caller cannot see.
    throw new HttpError(404, 'rebuild not found');
  }
  return { workspace, job: found };
}

export function rebuildRoutes(config: RebuildRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuild`,
      methods: ['POST'],
      scope: 'rebuilds:write',
      handler: async (c) => submit(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuild/preview`,
      methods: ['POST'],
      scope: 'rebuilds:read',
      handler: async (c) => {
        const { workspace } = await workspaceAccess(c, config.store, 'read');
        if (config.repos === undefined) {
          throw new HttpError(503, 'this deployment holds no repositories');
        }
        return jsonResponse({
          patches: await previewRebuild(
            { store: config.store, repos: config.repos, clock: config.clock },
            workspace,
          ),
        });
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuilds`,
      scope: 'rebuilds:read',
      handler: async (c) => {
        const { workspace } = await workspaceAccess(c, config.store, 'read');
        const jobs = await rebuildsOf(config.store, workspace.slug);
        return jsonResponse(jobs.map((one) => serialiseRebuild(one)));
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuilds/:id`,
      scope: 'rebuilds:read',
      handler: async (c) => jsonResponse(serialiseRebuild((await namedJob(c, config, 'read')).job)),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuilds/:id`,
      methods: ['DELETE'],
      scope: 'rebuilds:write',
      handler: async (c) => {
        const found = (await namedJob(c, config, 'write')).job;
        if (found.status !== 'queued') {
          throw new HttpError(409, `a ${found.status} rebuild cannot be cancelled`);
        }
        await updateRebuild(config.store, found.id, {
          status: 'cancelled',
          finished_at: config.clock.now().toISOString(),
        });
        return jsonResponse(
          serialiseRebuild((await rebuildById(config.store, found.id)) as RebuildRecord),
        );
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuilds/:id/requeue`,
      methods: ['POST'],
      scope: 'rebuilds:write',
      handler: async (c) => {
        const { workspace, job: found } = await namedJob(c, config, 'write');
        if (found.status !== 'dead_letter') {
          throw new HttpError(409, `a ${found.status} rebuild cannot be queued again`);
        }
        if ((await pendingRebuild(config.store, workspace.slug)) !== undefined) {
          throw new HttpError(
            409,
            'a rebuild of this workspace is already under way',
            'concurrent_rebuild',
          );
        }
        await updateRebuild(config.store, found.id, {
          status: 'queued',
          error: null,
          finished_at: null,
        });
        if (config.jobs !== undefined) {
          await queueRebuild(config.jobs, found.id);
        }
        return jsonResponse(
          serialiseRebuild((await rebuildById(config.store, found.id)) as RebuildRecord),
        );
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/rebuilds/:id/rollback`,
      methods: ['POST'],
      scope: 'rebuilds:write',
      handler: async (c) => {
        const { workspace, job: found } = await namedJob(c, config, 'write');
        if (found.status !== 'completed' || found.previous_integration_head === null) {
          throw new HttpError(409, 'this rebuild has no previous integration head to return to');
        }
        if (config.repos === undefined) {
          throw new HttpError(503, 'this deployment holds no repositories');
        }
        const repos = config.repos;
        const previous = found.previous_integration_head;

        await withWorkspaceLock(config.locks, workspace.slug, async () => {
          try {
            await repos.updateRefs(repositoryOf(workspace), [
              {
                name: `refs/heads/${workspace.integration_branch ?? 'integration'}`,
                oldOid: null,
                newOid: previous,
                force: true,
              },
            ]);
          } catch (err) {
            throw new HttpError(
              500,
              `the integration branch could not be moved back: ${err instanceof Error ? err.message : 'the store refused'}`,
            );
          }
        });
        return jsonResponse({ rolled_back_to: previous });
      },
    },
  ];
}
