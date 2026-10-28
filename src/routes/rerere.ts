import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import { resolutionStore } from '../history/resolutions.js';
import { repositoryOf } from '../repo/repository.js';
import { workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';

/**
 * The conflict resolutions the hub remembers.
 *
 * A resolution is only useful because the next rebuild replays it, so
 * the two things a person needs are to see what will be replayed and
 * to forget one that has outlived its usefulness. Forgetting is the
 * dangerous half: the next occurrence of that conflict is reported
 * rather than settled, which is exactly what someone asks for when a
 * recorded resolution has gone stale.
 */
export interface RerereRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
}

async function listResolutions(c: Context, config: RerereRouteConfig): Promise<Response> {
  const { workspace } = await workspaceAccess(c, config.store, 'read');
  const held = await resolutionStore(config.store, config.clock).list(repositoryOf(workspace));
  return jsonResponse({
    resolutions: held.map((one) => ({
      id: one.identity,
      path: one.path ?? null,
      recorded_at: one.recordedAt,
      resolved: one.resolved,
    })),
  });
}

/**
 * What a pathspec names. The CLI sends a path with its slashes
 * intact and everything else escaped, so a pathspec that decodes to
 * nothing is a client error rather than a path nobody has.
 */
export function readPathspec(raw: string): string {
  let decoded: string;
  try {
    decoded = decodeURIComponent(raw);
  } catch {
    throw new HttpError(400, 'the pathspec is not a valid escaped path');
  }
  if (decoded.trim().length === 0) {
    throw new HttpError(400, 'a pathspec must name an identity or a path');
  }
  return decoded;
}

async function forgetResolution(c: Context, config: RerereRouteConfig): Promise<Response> {
  // Checked before the workspace is looked up: a malformed pathspec
  // is the caller's mistake whatever workspace it names.
  const pathspec = readPathspec(c.req.param('pathspec') ?? '');

  const { workspace } = await workspaceAccess(c, config.store, 'write');
  const repository = repositoryOf(workspace);
  const resolutions = resolutionStore(config.store, config.clock);

  const forgotten = await withWorkspaceLock(config.locks, workspace.slug, async () => {
    // An identity is 64 hex characters; anything else is a path.
    if (/^[0-9a-f]{64}$/.test(pathspec)) {
      return (await resolutions.forget(repository, pathspec)) ? 1 : 0;
    }
    return resolutions.forgetPath(repository, pathspec);
  });

  if (forgotten === 0) {
    throw new HttpError(404, 'no recorded resolution matches that identity or path');
  }
  return new Response(null, { status: 204 });
}

export function rerereRoutes(config: RerereRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/rerere`,
      scope: 'rebuilds:read',
      handler: async (c) => listResolutions(c, config),
    },
    {
      // The pathspec keeps its slashes, so it is the rest of the path
      // rather than one segment: a resolution is recorded at
      // src/config.go, and that is what a person deletes.
      path: `${config.mountPoint}/workspaces/:slug/rerere/:pathspec{.+}`,
      methods: ['DELETE'],
      scope: 'rebuilds:write',
      handler: async (c) => forgetResolution(c, config),
    },
  ];
}
