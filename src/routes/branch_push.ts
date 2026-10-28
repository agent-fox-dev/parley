import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { registeredPatches } from '../patches/registry.js';
import { repositoryOf } from '../repo/repository.js';
import { headRef } from '../git/after_push.js';
import { workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import {
  branchNameRefusal,
  inRepositoryRefusal,
  readPushBody,
  readinessRefusal,
  unpushableRefusal,
} from '../push/checks.js';
import { pushBranch } from '../push/branch_push.js';

/**
 * Pushing one branch of the hub's copy to origin.
 *
 * The order of the checks is the behaviour, so the handler reads as
 * the reference's own list: the scope before the workspace, the body
 * before the name, the branches this route will not send before the
 * lock, and the repository's own protections inside it.
 */
export interface BranchPushRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  secrets?: SecretStore;
  repos?: RepositoryStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

/**
 * Asks the platform to let the work finish whatever the caller does.
 * Not every runtime offers it, and the one that does not cancels
 * nothing, so its absence is not a failure.
 */
function keepRunning(c: Context, work: Promise<unknown>): void {
  try {
    c.executionCtx.waitUntil(work.catch(() => undefined));
  } catch {
    // No execution context here: nothing cancels the work either.
  }
}

async function push(c: Context, config: BranchPushRouteConfig): Promise<Response> {
  // 2. The workspace and write access. The scope came first, in the
  // router, which checks it before this handler is reached.
  const { workspace } = await workspaceAccess(c, config.store, 'write');

  // 3. The body, then the name.
  const body = await c.req
    .json()
    .catch(() => undefined)
    .then((raw: unknown) => readPushBody(raw));
  const name = c.req.param('name') ?? '';
  const invalid = branchNameRefusal(name);
  if (invalid !== undefined) {
    throw invalid;
  }

  // 4. Readiness.
  const unready = readinessRefusal(workspace);
  if (unready !== undefined) {
    throw unready;
  }

  // 5. The branches this route will not send, decided before the lock
  // so that a busy workspace gives the same answer as a free one.
  const patches = await registeredPatches(config.store, workspace.slug);
  const unpushable = unpushableRefusal(workspace, name, patches, body.force);
  if (unpushable !== undefined) {
    throw unpushable;
  }

  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }
  const repos = config.repos;

  // 6. The lock, without waiting.
  return withWorkspaceLock(config.locks, workspace.slug, async () => {
    const refs = await repos.listRefs(repositoryOf(workspace));
    const head = headRef(refs)?.name.slice('refs/heads/'.length);

    // 7. The protections that need the repository.
    const protection = inRepositoryRefusal({
      workspace,
      name,
      force: body.force,
      ...(head === undefined ? {} : { head }),
      present: refs.some((ref) => ref.name === `refs/heads/${name}`),
    });
    if (protection !== undefined) {
      throw protection;
    }

    // 8. The push, which outlives its caller.
    //
    // A platform that cancels work when a client disconnects would
    // otherwise leave a branch half sent and nothing recorded, so the
    // work is handed to the execution context first and awaited
    // second: the answer is the same, and the push finishes either
    // way.
    const work = pushBranch(
      {
        repos,
        ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
        ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
        ...(config.log === undefined ? {} : { log: config.log }),
      },
      workspace,
      { name, force: body.force },
    );
    keepRunning(c, work);
    return jsonResponse(await work);
  });
}

export function branchPushRoutes(config: BranchPushRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/branches/:name/push`,
      methods: ['POST'],
      scope: 'workspaces:write',
      handler: async (c) => push(c, config),
    },
  ];
}
