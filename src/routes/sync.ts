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
import { syncStandard } from '../sync/standard.js';
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
    await syncStandard(
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
