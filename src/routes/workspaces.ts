import { type EventRecorder, actorIn, record } from '../audit/recorder.js';
import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse, readJsonObject } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { ensureRepository } from '../repo/repository.js';
import { checkRemoteCredential } from '../repo/copy_in.js';
import { queueCopyIn } from '../jobs/copy_in.js';
import type { JobQueue } from '../platform/jobs.js';
import { findOrg, findUser, isMember, orgsOfUser } from '../store/identity.js';
import type { OrgRecord } from '../store/identity.js';
import {
  type WorkspaceRecord,
  type WorkspaceChanges,
  findWorkspace,
  insertWorkspace,
  listVisibleWorkspaces,
  updateWorkspace,
} from '../store/workspaces.js';
import { accessLevel, hidesMissingScope, workspaceAccess } from '../workspace/access.js';
import {
  archiveWorkspace,
  deleteWorkspaceAndTokens,
  reactivateWorkspace,
} from '../workspace/lifecycle.js';
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
  locks: Locks;
  /** Absent where the deployment was given no repository store. */
  repos?: RepositoryStore;
  jobs?: JobQueue;
  /** Injected so a test reaches a remote of its own. */
  fetch?: typeof fetch;
  /** Where this group writes down what it did. */
  events?: EventRecorder;
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

  // The remote is asked about the credential before anything is written.
  // Only a refusal stops the create: a forge having a bad day is not a
  // bad request, and the copy will record what it finds.
  if (config.repos !== undefined) {
    const credential =
      request.credentials['GIT_PAT'] !== undefined
        ? ({ kind: 'token', token: request.credentials['GIT_PAT'] } as const)
        : request.credentials['GIT_USERNAME'] !== undefined &&
            request.credentials['GIT_PASSWORD'] !== undefined
          ? ({
              kind: 'basic',
              username: request.credentials['GIT_USERNAME'],
              password: request.credentials['GIT_PASSWORD'],
            } as const)
          : undefined;

    const answer = await checkRemoteCredential({
      gitUrl: request.git_url,
      ...(credential === undefined ? {} : { credential }),
      ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
    });
    if (answer === 'refused') {
      throw new HttpError(400, 'the repository refused the credentials given');
    }
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

  // The repository exists from the moment the workspace does. Copying
  // the remote into it is the next task's work and moves the status on.
  if (config.repos !== undefined) {
    await ensureRepository(config.repos, workspace);
    if (config.jobs !== undefined) {
      await queueCopyIn(config.jobs, workspace.slug);
    }
  }

  await record(config.events, {
    type: 'hub.workspace.create',
    workspace: workspace.slug,
    resourceId: workspace.slug,
    actor: actorIn(c),
    metadata: { git_url: workspace.git_url, branch: workspace.branch ?? undefined },
  });

  return jsonResponse(
    serialiseWorkspace(workspace, {
      access: 'owner',
      orgSlug: org.slug,
      externalUrl: config.externalUrl,
    }),
    201,
  );
}

/** One workspace, for any caller the access decision admits to read it. */
async function readWorkspace(c: Context, config: WorkspaceRouteConfig): Promise<Response> {
  const { workspace, level } = await workspaceAccess(c, config.store, 'read');
  const org = workspace.org_id === null ? undefined : await findOrg(config.store, workspace.org_id);

  return jsonResponse(
    serialiseWorkspace(workspace, {
      access: level,
      orgSlug: org?.slug ?? null,
      externalUrl: config.externalUrl,
    }),
  );
}

/**
 * The workspaces the caller may see. The access field of each is the level
 * the one decision gives, so a list and a read never disagree.
 */
async function listWorkspaces(c: Context, config: WorkspaceRouteConfig): Promise<Response> {
  const identity = identityOf(c);
  const includeArchived = c.req.query('include_archived') === 'true';
  const boundTo = identity?.boundTo;

  const orgs =
    identity?.userId === undefined ? [] : await orgsOfUser(config.store, identity.userId);
  const workspaces = await listVisibleWorkspaces(config.store, {
    ...(identity?.kind === 'admin_token' ? {} : { ownerId: identity?.userId ?? '' }),
    orgIds: orgs.map((org) => org.id),
    includeArchived,
  });

  const slugById = new Map(orgs.map((org) => [org.id, org.slug]));
  const serialised = [];
  for (const workspace of workspaces) {
    // A bound token sees at most its own workspace, whatever else it owns.
    if (boundTo !== undefined && workspace.slug !== boundTo) {
      continue;
    }
    const level = (await accessLevel(config.store, identity, workspace)) ?? 'member';
    const known = workspace.org_id === null ? undefined : slugById.get(workspace.org_id);
    const orgSlug =
      known ??
      (workspace.org_id === null
        ? null
        : ((await findOrg(config.store, workspace.org_id))?.slug ?? null));
    serialised.push(
      serialiseWorkspace(workspace, { access: level, orgSlug, externalUrl: config.externalUrl }),
    );
  }
  return jsonResponse(serialised);
}

/** The fields a workspace is created with and never changes afterwards. */
const IMMUTABLE = [
  'slug',
  'git_url',
  'branch',
  'owner_id',
  'workspace_mode',
  'upstream_url',
  'integration_branch',
];

const MAX_DISPLAY_NAME = 128;
const MAX_DESCRIPTION = 1024;

/** A text field an update may change, or the default it returns to. */
function changedText(
  body: Record<string, unknown>,
  field: string,
  limit: number,
  fallback: string,
): string | undefined {
  if (!(field in body)) {
    return undefined;
  }
  const value = body[field];
  if (value === null) {
    return fallback;
  }
  if (typeof value !== 'string' || value.length > limit) {
    throw new HttpError(400, `${field} must be a string of at most ${limit} characters`);
  }
  return value;
}

async function changeWorkspace(c: Context, config: WorkspaceRouteConfig): Promise<Response> {
  const { workspace, level } = await workspaceAccess(c, config.store, 'write');
  // Access first, then the status: a member writing to an archived
  // workspace has already been refused by the line above.
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'this workspace is archived; reactivate it first');
  }

  const body = await readJsonObject(c);
  if (Object.keys(body).length === 0) {
    throw new HttpError(400, 'name at least one field to change');
  }
  for (const field of IMMUTABLE) {
    if (field in body) {
      throw new HttpError(400, `${field} cannot be changed`);
    }
  }
  if ('sync_mode' in body) {
    throw new HttpError(400, 'sync_mode was retired; use the sync_enabled setting');
  }

  const changes: WorkspaceChanges = {};
  const displayName = changedText(body, 'display_name', MAX_DISPLAY_NAME, workspace.slug);
  if (displayName !== undefined) {
    changes.display_name = displayName;
  }
  const description = changedText(body, 'description', MAX_DESCRIPTION, '');
  if (description !== undefined) {
    changes.description = description;
  }
  if ('settings' in body) {
    changes.settings = checkedSettings(body['settings'], workspace.settings);
  }

  let orgSlug: string | null = null;
  if ('org_id' in body) {
    if (body['org_id'] === null) {
      changes.org_id = null;
    } else {
      // The organization is the owner's to belong to, whoever is asking.
      const org = await owningOrg(config.store, workspace.owner_id, String(body['org_id']));
      changes.org_id = org.id;
      orgSlug = org.slug;
    }
  }

  await updateWorkspace(config.store, workspace.slug, changes, config.clock.now().toISOString());

  await record(config.events, {
    type: 'hub.workspace.update',
    workspace: workspace.slug,
    resourceId: workspace.slug,
    actor: actorIn(c),
    metadata: { fields: Object.keys(changes).toSorted() },
  });

  const updated = await findWorkspace(config.store, workspace.slug);
  const org =
    updated?.org_id === null || updated?.org_id === undefined
      ? undefined
      : await findOrg(config.store, updated.org_id);

  return jsonResponse(
    serialiseWorkspace(updated ?? workspace, {
      access: level,
      orgSlug: org?.slug ?? orgSlug,
      externalUrl: config.externalUrl,
    }),
  );
}

/** Answers with the workspace as it stands after a change to its status. */
async function answerWithWorkspace(
  config: WorkspaceRouteConfig,
  slug: string,
  access: 'admin' | 'owner' | 'member',
): Promise<Response> {
  const updated = await findWorkspace(config.store, slug);
  if (updated === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  const org = updated.org_id === null ? undefined : await findOrg(config.store, updated.org_id);
  return jsonResponse(
    serialiseWorkspace(updated, {
      access,
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
      path: `${config.mountPoint}/workspaces`,
      scope: 'workspaces:read',
      hideScopeFailure: hidesMissingScope('GET', '/workspaces'),
      handler: async (c) => listWorkspaces(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug`,
      scope: 'workspaces:read',
      hideScopeFailure: hidesMissingScope('GET', '/workspaces/:slug'),
      handler: async (c) => readWorkspace(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug`,
      methods: ['PATCH'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('PATCH', '/workspaces/:slug'),
      handler: async (c) => changeWorkspace(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/archive`,
      methods: ['POST'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('POST', '/workspaces/:slug/archive'),
      handler: async (c) => {
        const { workspace, level } = await workspaceAccess(c, config.store, 'write');
        await archiveWorkspace(config.store, config.locks, config.clock, workspace, {
          ...(config.repos === undefined ? {} : { repos: config.repos }),
          ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
          ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
          ...(config.events === undefined ? {} : { events: config.events }),
        });
        await record(config.events, {
          type: 'hub.workspace.archive',
          workspace: workspace.slug,
          resourceId: workspace.slug,
          actor: actorIn(c),
          metadata: { head_sha: workspace.head_sha ?? undefined },
        });
        return answerWithWorkspace(config, workspace.slug, level);
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/reactivate`,
      methods: ['POST'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('POST', '/workspaces/:slug/reactivate'),
      handler: async (c) => {
        const { workspace, level } = await workspaceAccess(c, config.store, 'write');
        await reactivateWorkspace(config.store, config.clock, workspace);
        await record(config.events, {
          type: 'hub.workspace.reactivate',
          workspace: workspace.slug,
          resourceId: workspace.slug,
          actor: actorIn(c),
        });
        return answerWithWorkspace(config, workspace.slug, level);
      },
    },
    {
      path: `${config.mountPoint}/workspaces/:slug`,
      methods: ['DELETE'],
      scope: 'workspaces:write',
      hideScopeFailure: hidesMissingScope('DELETE', '/workspaces/:slug'),
      handler: async (c) => {
        const { workspace } = await workspaceAccess(c, config.store, 'write');
        const revoked = await deleteWorkspaceAndTokens(config.store, config.clock, workspace, {
          ...(config.repos === undefined ? {} : { repos: config.repos }),
          ...(config.events === undefined ? {} : { events: config.events }),
        });
        await record(config.events, {
          type: 'hub.workspace.delete',
          workspace: workspace.slug,
          resourceId: workspace.slug,
          actor: actorIn(c),
          metadata: { revoked_tokens: revoked },
        });
        return emptyResponse(204);
      },
    },
  ];
}
