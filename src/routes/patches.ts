import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { newIdentifier } from '../auth/credentials.js';
import {
  type NewPatch,
  type PatchChanges,
  type PatchRecord,
  type PatchStatus,
  changePatch,
  insertPatch,
  livePatches,
  patchByBranch,
  patchById,
  serialisePatch,
} from '../patch/list.js';
import { repositoryOf } from '../repo/repository.js';
import { type BranchResolution, resolveBranch } from '../patch/resolve_branch.js';
import { workspaceAccess } from '../workspace/access.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import { effectiveSettings } from '../workspace/settings.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * The patch list of a carry-patch workspace.
 *
 * The add is the exacting one. A batch checks every element's syntax
 * first, in array order, and only then looks up any branch, so a batch
 * whose first element names a branch nobody has and whose second asks
 * for position 0 is refused for the position: a client fixing one
 * mistake at a time should be told about the cheap one first.
 */
export interface PatchRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  locks: Locks;
  secrets?: SecretStore;
  repos?: RepositoryStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

interface AddRequest extends NewPatch {
  skip_branch_check?: boolean;
  if_not_exists?: boolean;
}

/** The workspace a patch route names, which must carry patches at all. */
async function carryPatchWorkspace(
  c: Context,
  config: PatchRouteConfig,
  need: 'read' | 'write',
): Promise<{ workspace: WorkspaceRecord }> {
  const { workspace } = await workspaceAccess(c, config.store, need);
  return { workspace };
}

/** What one element of an add asks for, checked for syntax alone. */
function readElement(raw: unknown, workspace: WorkspaceRecord, label: string): AddRequest {
  const where = label.length === 0 ? '' : `${label}: `;
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, `${where}a patch must be a JSON object`);
  }
  const body = raw as Record<string, unknown>;

  const branch = body['branch_name'];
  if (typeof branch !== 'string' || branch.length === 0) {
    throw new HttpError(400, `${where}branch_name is required`);
  }
  if (branch === workspace.integration_branch) {
    throw new HttpError(400, `${where}the integration branch cannot be carried as a patch`);
  }

  const position = body['position'];
  if (position !== undefined) {
    if (typeof position !== 'number' || !Number.isInteger(position)) {
      throw new HttpError(400, `${where}position must be a whole number`);
    }
    if (position < 1) {
      throw new HttpError(400, `${where}position must be >= 1`);
    }
  }

  for (const name of ['upstream_pr_url', 'description'] as const) {
    if (body[name] !== undefined && typeof body[name] !== 'string') {
      throw new HttpError(400, `${where}${name} must be a string`);
    }
  }
  for (const name of ['skip_branch_check', 'if_not_exists'] as const) {
    if (body[name] !== undefined && typeof body[name] !== 'boolean') {
      throw new HttpError(400, `${where}${name} must be true or false`);
    }
  }

  return {
    branch_name: branch,
    ...(typeof position === 'number' ? { position } : {}),
    ...(typeof body['upstream_pr_url'] === 'string'
      ? { upstream_pr_url: body['upstream_pr_url'] }
      : {}),
    ...(typeof body['description'] === 'string' ? { description: body['description'] } : {}),
    ...(body['skip_branch_check'] === true ? { skip_branch_check: true } : {}),
    ...(body['if_not_exists'] === true ? { if_not_exists: true } : {}),
  };
}

async function addPatches(c: Context, config: PatchRouteConfig): Promise<Response> {
  const { workspace } = await carryPatchWorkspace(c, config, 'write');
  if (workspace.workspace_mode !== 'carry_patch') {
    throw new HttpError(400, 'only a carry-patch workspace carries patches');
  }
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'this workspace is archived');
  }

  const raw: unknown = await c.req.json().catch(() => undefined);
  const batch = Array.isArray(raw);
  const elements = batch ? raw : [raw];
  if (batch && elements.length === 0) {
    throw new HttpError(400, 'a batch must hold at least one patch');
  }

  // Every element's syntax, in array order, before any branch is
  // looked up.
  const asked = elements.map((element, index) =>
    readElement(element, workspace, batch ? `patch[${String(index)}]` : ''),
  );

  const named = new Set<string>();
  for (const [index, element] of asked.entries()) {
    if (named.has(element.branch_name)) {
      throw new HttpError(
        409,
        `${batch ? `patch[${String(index)}]: ` : ''}branch "${element.branch_name}" is named twice in this batch`,
      );
    }
    named.add(element.branch_name);
  }

  // Then the list, which is the other cheap check.
  for (const [index, element] of asked.entries()) {
    const held = await patchByBranch(config.store, workspace.slug, element.branch_name);
    if (held === undefined) {
      continue;
    }
    if (!batch && element.if_not_exists === true) {
      return jsonResponse(serialisePatch(held));
    }
    throw new HttpError(
      409,
      `${batch ? `patch[${String(index)}]: ` : ''}branch "${element.branch_name}" is already a patch of this workspace`,
    );
  }

  if (config.repos === undefined) {
    throw new HttpError(503, 'this deployment holds no repositories');
  }
  const repos = config.repos;
  const fromOrigin = effectiveSettings(workspace.settings)['patch_branch_source'] === 'origin';

  // Then every branch, before anything is added. A branch that has to
  // be created needs the repository, so the lock is taken around the
  // whole resolution rather than around each one.
  const resolutions = await withWorkspaceLock(config.locks, workspace.slug, async () => {
    const found: BranchResolution[] = [];
    for (const [index, element] of asked.entries()) {
      if (element.skip_branch_check === true) {
        found.push('skipped');
        continue;
      }
      try {
        found.push(
          await resolveBranch(
            {
              repos,
              ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
              ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
              ...(config.log === undefined ? {} : { log: config.log }),
            },
            workspace,
            element.branch_name,
            { fromOrigin },
          ),
        );
      } catch (err) {
        if (batch && err instanceof HttpError) {
          throw new HttpError(err.status, `patch[${String(index)}]: ${err.message}`, err.errorType);
        }
        throw err;
      }
    }
    return found;
  });

  const at = config.clock.now().toISOString();
  const added: PatchRecord[] = [];
  for (const element of asked) {
    added.push(await insertPatch(config.store, workspace.slug, element, at, newIdentifier()));
  }

  const body = batch
    ? added.map((patch) => serialisePatch(patch))
    : serialisePatch(added[0] as PatchRecord, {});
  return jsonResponse(
    batch
      ? body
      : { ...(body as Record<string, unknown>), branch_resolution: resolutions[0] ?? 'skipped' },
    201,
  );
}

async function listPatches(c: Context, config: PatchRouteConfig): Promise<Response> {
  const { workspace } = await carryPatchWorkspace(c, config, 'read');
  if (workspace.workspace_mode !== 'carry_patch') {
    return jsonResponse([]);
  }
  const patches = await livePatches(config.store, workspace.slug);
  return jsonResponse(patches.map((patch) => serialisePatch(patch)));
}

/**
 * The tip the hub kept when it replaced this patch's branch, where it
 * kept one. Reading it is best effort: a workspace with no repository
 * and a store that cannot answer both mean "no backup to offer", and
 * neither is worth failing the read over.
 */
async function replacedTip(
  config: PatchRouteConfig,
  workspace: WorkspaceRecord,
  branch: string,
): Promise<string | undefined> {
  if (config.repos === undefined) {
    return undefined;
  }
  try {
    const refs = await config.repos.listRefs(repositoryOf(workspace));
    return refs.find((ref) => ref.name === `refs/hub/replaced/${branch}`)?.oid;
  } catch {
    return undefined;
  }
}

async function readPatch(c: Context, config: PatchRouteConfig): Promise<Response> {
  const { workspace } = await carryPatchWorkspace(c, config, 'read');
  const patch = await patchById(config.store, workspace.slug, c.req.param('id') ?? '');
  if (patch === undefined) {
    throw new HttpError(404, 'patch not found');
  }
  const replaced = await replacedTip(config, workspace, patch.branch_name);
  return jsonResponse(
    serialisePatch(patch, replaced === undefined ? {} : { replaced_sha: replaced }),
  );
}

const STATUSES: readonly PatchStatus[] = ['active', 'merged_upstream', 'conflict', 'disabled'];

/** What a change asks for, with anything it may not change left out. */
function readChanges(raw: unknown, held: number): PatchChanges {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;
  const changes: PatchChanges = {};

  if (body['position'] !== undefined) {
    const position = body['position'];
    if (typeof position !== 'number' || !Number.isInteger(position) || position < 1) {
      throw new HttpError(400, 'position must be a whole number of at least 1');
    }
    if (position > held) {
      throw new HttpError(400, `position must be no more than ${String(held)}`);
    }
    changes.position = position;
  }

  if (body['status'] !== undefined) {
    const status = body['status'];
    if (status === 'deleted') {
      throw new HttpError(400, 'a patch is deleted by removing it, not by setting its status');
    }
    if (typeof status !== 'string' || !STATUSES.includes(status as PatchStatus)) {
      throw new HttpError(400, `status must be one of ${STATUSES.join(', ')}`);
    }
    changes.status = status as PatchStatus;
  }

  for (const name of ['description', 'upstream_pr_url'] as const) {
    const value = body[name];
    if (value === undefined) {
      continue;
    }
    if (value !== null && typeof value !== 'string') {
      throw new HttpError(400, `${name} must be a string or null`);
    }
    changes[name] = value;
  }

  // branch_name is ignored rather than refused: a patch is the same
  // patch whatever else about it changes.
  return changes;
}

async function changeOnePatch(c: Context, config: PatchRouteConfig): Promise<Response> {
  const { workspace } = await carryPatchWorkspace(c, config, 'write');
  const patch = await patchById(config.store, workspace.slug, c.req.param('id') ?? '');
  if (patch === undefined) {
    throw new HttpError(404, 'patch not found');
  }
  if (patch.status === 'deleted') {
    throw new HttpError(409, 'this patch was removed; restore it before changing it');
  }

  const live = await livePatches(config.store, workspace.slug);
  const changes = readChanges(await c.req.json().catch(() => undefined), live.length);
  const changed = await changePatch(
    config.store,
    workspace.slug,
    patch,
    changes,
    config.clock.now().toISOString(),
  );
  return jsonResponse(serialisePatch(changed));
}

export function patchRoutes(config: PatchRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/patches`,
      methods: ['POST'],
      scope: 'patches:write',
      handler: async (c) => addPatches(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/patches`,
      scope: 'patches:read',
      handler: async (c) => listPatches(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/patches/:id`,
      scope: 'patches:read',
      handler: async (c) => readPatch(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/patches/:id`,
      methods: ['PATCH'],
      scope: 'patches:write',
      handler: async (c) => changeOnePatch(c, config),
    },
  ];
}
