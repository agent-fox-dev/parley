import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { findOrg } from '../store/identity.js';
import { findWorkspace } from '../store/workspaces.js';
import { hidesMissingScope, workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import { serialiseWorkspace } from '../workspace/serialise.js';
import { recloneRefusal, recloneWorkspace } from '../sync/reclone.js';

export interface RecloneRouteConfig {
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

async function reclone(c: Context, config: RecloneRouteConfig): Promise<Response> {
  const { workspace, level } = await workspaceAccess(c, config.store, 'write');

  const refusal = recloneRefusal(workspace);
  if (refusal !== undefined) {
    throw refusal;
  }
  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }
  const repos = config.repos;

  await withWorkspaceLock(config.locks, workspace.slug, async () => {
    await recloneWorkspace(
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
  });

  const updated = await findWorkspace(config.store, workspace.slug);
  if (updated === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  const org = updated.org_id === null ? undefined : await findOrg(config.store, updated.org_id);
  return jsonResponse(
    serialiseWorkspace(updated, {
      access: level,
      orgSlug: org?.slug ?? null,
      externalUrl: config.externalUrl,
    }),
  );
}

export function recloneRoutes(config: RecloneRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/reclone`,
      methods: ['POST'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('POST', '/workspaces/:slug/reclone'),
      handler: async (c) => reclone(c, config),
    },
  ];
}
