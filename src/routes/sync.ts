import { type EventRecorder, actorIn, record } from '../audit/recorder.js';
import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import type { LogSink } from '../http/router.js';
import { findOrg } from '../store/identity.js';
import { findWorkspace } from '../store/workspaces.js';
import { hidesMissingScope, workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import { serialiseWorkspace } from '../workspace/serialise.js';
import { syncRefusal } from '../sync/preconditions.js';
import { jsonResponse } from '../http/json.js';
import { type SyncResult, syncStandard } from '../sync/standard.js';
import { type CarryPatchSync, syncCarryPatch } from '../sync/carry_patch.js';

/**
 * Bringing a workspace up to date with its remote.
 *
 * A carry_patch workspace fetches upstream and works on its patch list
 * instead of fast-forwarding a branch. That is the carry-patch
 * package's, and the branch point is here: this route decides by the
 * workspace's mode and calls the standard sync for a standard
 * workspace alone.
 */
export interface SyncRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  secrets?: SecretStore;
  repos?: RepositoryStore;
  jobs?: JobQueue;
  externalUrl?: string;
  fetch?: typeof fetch;
  log?: LogSink;
  /** Where this group writes down what it did. */
  events?: EventRecorder;
}

/**
 * What a carry-patch sync did to the patch branches: one event for the
 * sync as a whole, naming the branches in each state, and one more for
 * every branch whose tip it discarded.
 */
async function recordCarried(
  c: Context,
  config: SyncRouteConfig,
  slug: string,
  carried: CarryPatchSync,
): Promise<void> {
  const synced = carried.patches_synced;
  if (synced === undefined) {
    return;
  }
  const named = (action: string) =>
    synced.filter((one) => one.action === action).map((one) => one.branch_name);

  await record(config.events, {
    type: 'hub.patch.sync',
    workspace: slug,
    resourceId: slug,
    actor: actorIn(c),
    metadata: {
      origin_fetched: carried.origin_fetched,
      created: named('created'),
      fast_forwarded: named('fast_forwarded'),
      replaced: named('replaced'),
      diverged: carried.patches_diverged ?? [],
      missing_on_origin: named('missing_on_origin'),
    },
  });

  for (const one of synced.filter((patch) => patch.action === 'replaced')) {
    await record(config.events, {
      type: 'hub.patch.replace',
      workspace: slug,
      resourceId: one.branch_name,
      actor: actorIn(c),
      metadata: {
        branch_name: one.branch_name,
        replaced_sha: one.replaced_sha,
        origin_sha: one.origin_sha,
      },
    });
  }
}

async function sync(c: Context, config: SyncRouteConfig): Promise<Response> {
  const { workspace, level } = await workspaceAccess(c, config.store, 'write');

  const refusal = syncRefusal(workspace);
  if (refusal !== undefined) {
    throw refusal;
  }
  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }
  const repos = config.repos;

  const query = new URL(c.req.url).searchParams;
  const resetToUpstream = query.get('reset_to_upstream') === 'true';
  // A suite needs a deadline it can reach; a caller cannot lengthen one.
  const asked = Number(query.get('deadline_ms') ?? '');
  const deadlineMs = Number.isFinite(asked) && asked > 0 ? asked : undefined;

  // What a carry-patch sync found, which is answered beside the
  // workspace itself.
  let carried: CarryPatchSync | undefined;
  /** What a standard sync made of the fork, for the event it records. */
  let result: SyncResult | undefined;

  // The lock, not a status field, is what makes a second sync wait: a
  // sync in progress is not a state the workspace stores.
  await withWorkspaceLock(config.locks, workspace.slug, async () => {
    if (workspace.workspace_mode === 'carry_patch') {
      // The carry-patch sync is a different operation, not the
      // standard one with extras: it fetches upstream, moves no
      // branch of its own and reports its findings.
      carried = await syncCarryPatch(
        {
          store: config.store,
          repos,
          clock: config.clock,
          ...(config.jobs === undefined ? {} : { jobs: config.jobs }),
          ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
          ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
          ...(config.log === undefined ? {} : { log: config.log }),
        },
        workspace,
      );
      return;
    }
    result = await syncStandard(
      {
        store: config.store,
        repos,
        clock: config.clock,
        ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
        ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
        ...(config.log === undefined ? {} : { log: config.log }),
        ...(deadlineMs === undefined ? {} : { deadlineMs }),
      },
      workspace,
      { resetToUpstream },
    );
  });

  // Recorded after the lock is let go and only for a sync that
  // succeeded: a failure throws above and records nothing.
  if (result !== undefined) {
    await record(config.events, {
      type: 'hub.workspace.sync',
      workspace: workspace.slug,
      resourceId: workspace.slug,
      actor: actorIn(c),
      metadata: { result },
    });
  }
  if (carried !== undefined) {
    await recordCarried(c, config, workspace.slug, carried);
  }

  const updated = await findWorkspace(config.store, workspace.slug);
  if (updated === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  const org = updated.org_id === null ? undefined : await findOrg(config.store, updated.org_id);
  return jsonResponse({
    ...serialiseWorkspace(updated, {
      access: level,
      orgSlug: org?.slug ?? null,
      externalUrl: config.externalUrl,
    }),
    ...carried,
  });
}

export function syncRoutes(config: SyncRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/sync`,
      methods: ['POST'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('POST', '/workspaces/:slug/sync'),
      handler: async (c) => sync(c, config),
    },
  ];
}
