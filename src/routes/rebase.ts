import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { rebaseBranches, readBatchBody } from '../merge/batch_rebase.js';
import { workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';

/**
 * Rebasing several branches onto a target while the caller waits.
 *
 * Unlike a merge job this does not wait its turn for the workspace: a
 * queued job has somewhere to wait and a synchronous request does
 * not, so a busy workspace is answered at once.
 */
export interface RebaseRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  repos?: RepositoryStore;
}

async function rebase(c: Context, config: RebaseRouteConfig): Promise<Response> {
  // The body first, as the reference states for this route too.
  const asked = readBatchBody(await c.req.json().catch(() => undefined));

  const { workspace } = await workspaceAccess(c, config.store, 'write');
  if (workspace.status !== 'ready') {
    throw new HttpError(400, 'this workspace is not ready to rebase');
  }
  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }
  const repos = config.repos;

  const results = await withWorkspaceLock(config.locks, workspace.slug, async () =>
    rebaseBranches({ store: config.store, repos, clock: config.clock }, workspace, asked),
  );
  return jsonResponse({ results });
}

export function rebaseRoutes(config: RebaseRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/rebase`,
      methods: ['POST'],
      scope: 'merges:write',
      handler: async (c) => rebase(c, config),
    },
  ];
}
