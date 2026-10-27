import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { findOrg, findUser, isMember, orgsOfUser } from '../store/identity.js';
import type { OrgRecord } from '../store/identity.js';
import { type WorkspaceRecord, findWorkspace, insertWorkspace } from '../store/workspaces.js';
import { serialiseWorkspace } from '../workspace/serialise.js';
import { checkedSettings } from '../workspace/settings.js';
import { checkedCreate } from '../workspace/validate.js';

export interface WorkspaceRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  /** Absent where the deployment configured no key to seal secrets with. */
  secrets?: SecretStore;
  externalUrl?: string;
}

/**
 * The organization the workspace belongs to: the one the body names, by id
 * or by slug, else the caller's own. The owner must belong to it either way.
 */
async function owningOrg(
  store: RelationalStore,
  ownerId: string,
  named: string | undefined,
): Promise<OrgRecord> {
  if (named === undefined) {
    const own = (await orgsOfUser(store, ownerId)).filter((org) => org.owner_id === ownerId);
    const personal = own[0];
    if (personal === undefined) {
      throw new HttpError(400, 'you have no personal organization; name an org_id');
    }
    return personal;
  }

  const byId = await findOrg(store, named);
  const org = byId ?? (await orgBySlug(store, named));
  if (org === undefined) {
    throw new HttpError(400, 'org_id names no organization');
  }
  if (!(await isMember(store, org.id, ownerId))) {
    throw new HttpError(403, 'you are not a member of that organization');
  }
  return org;
}

async function orgBySlug(store: RelationalStore, slug: string): Promise<OrgRecord | undefined> {
  const rows = await store.all<{ id: string }>('SELECT id FROM orgs WHERE slug = ?', [slug]);
  const id = rows[0]?.id;
  return id === undefined ? undefined : findOrg(store, id);
}

async function createWorkspace(c: Context, config: WorkspaceRouteConfig): Promise<Response> {
  const identity = identityOf(c);
  // A workspace needs a real user as its owner, and an admin token is not one.
  if (identity?.userId === undefined) {
    throw new HttpError(403, 'a workspace needs a real user as its owner');
  }
  const owner = await findUser(config.store, identity.userId);
  if (owner === undefined) {
    throw new HttpError(403, 'a workspace needs a real user as its owner');
  }

  const request = checkedCreate(await readJsonObject(c));
  const settings = checkedSettings(request.settings, {});
  const org = await owningOrg(config.store, owner.id, request.org_id);

  if ((await findWorkspace(config.store, request.slug)) !== undefined) {
    throw new HttpError(409, 'a workspace with this slug exists');
  }
  if (Object.keys(request.credentials).length > 0 && config.secrets === undefined) {
    throw new HttpError(500, 'this deployment cannot store repository credentials');
  }

  const at = config.clock.now().toISOString();
  const workspace: WorkspaceRecord = {
    slug: request.slug,
    git_url: request.git_url,
    branch: request.branch,
    owner_id: owner.id,
    org_id: org.id,
    // The record is written; copying the repository in is the repository
    // store's work, and it moves the status on from here.
    status: 'pending',
    display_name: request.display_name,
    description: request.description,
    head_sha: null,
    clone_error: null,
    upstream_head_sha: null,
    last_sync_at: null,
    sync_error: null,
    workspace_mode: request.workspace_mode,
    upstream_url: request.upstream_url,
    integration_branch: request.integration_branch,
    settings,
    created_at: at,
    updated_at: at,
  };
  await insertWorkspace(config.store, workspace);

  for (const [name, value] of Object.entries(request.credentials)) {
    await config.secrets?.put(workspaceScope(workspace.slug), name, value);
  }

  return jsonResponse(
    serialiseWorkspace(workspace, {
      access: 'owner',
      orgSlug: org.slug,
      externalUrl: config.externalUrl,
    }),
    201,
  );
}

/**
 * One workspace, for the caller who owns it. Widening this to the access
 * decision's three levels is what the access task does; until then a
 * caller who is not the owner is answered as though it did not exist.
 */
async function readWorkspace(c: Context, config: WorkspaceRouteConfig): Promise<Response> {
  const workspace = await findWorkspace(config.store, c.req.param('slug') ?? '');
  const identity = identityOf(c);
  if (workspace === undefined || workspace.owner_id !== identity?.userId) {
    throw new HttpError(404, 'workspace not found');
  }
  const org = workspace.org_id === null ? undefined : await findOrg(config.store, workspace.org_id);

  return jsonResponse(
    serialiseWorkspace(workspace, {
      access: 'owner',
      orgSlug: org?.slug ?? null,
      externalUrl: config.externalUrl,
    }),
  );
}

export function workspaceRoutes(config: WorkspaceRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces`,
      methods: ['POST'],
      scope: 'workspaces:write',
      handler: async (c) => createWorkspace(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug`,
      scope: 'workspaces:read',
      handler: async (c) => readWorkspace(c, config),
    },
  ];
}
