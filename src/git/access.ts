import type { Context } from 'hono';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Identity, identifyCredential } from '../auth/middleware.js';
import { grantsScope } from '../auth/scopes.js';
import { accessLevel } from '../workspace/access.js';
import { repositoryOf } from '../repo/repository.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import { findOrgByNameOrSlug } from '../store/identity.js';
import { asBearer } from './auth.js';

/**
 * A git request the service will not serve. It carries its own status
 * and message, because a git client is shown the message and a wrong
 * one tells the person to do the wrong thing.
 */
export class GitRefusal extends Error {
  constructor(
    readonly status: number,
    message: string,
    /** Headers the refusal needs, such as the Basic challenge. */
    readonly headers: Record<string, string> = {},
  ) {
    super(message);
    this.name = 'GitRefusal';
  }
}

export const NOT_FOUND = 'repository not found';
export const UNAUTHENTICATED = 'authentication required';
export const MEMBERS_READ_ONLY = 'org members have read-only access to this workspace';
export const NOT_AVAILABLE = 'repository not available';

export interface GitAccessDeps {
  store: RelationalStore;
  clock: Clock;
  /** The deployment's admin token, where one is configured. */
  adminToken?: string;
  /** Absent where the deployment was given no repository store. */
  repos?: RepositoryStore;
}

export interface GitSubject {
  workspace: WorkspaceRecord;
  repository: string;
  repos: RepositoryStore;
  identity: Identity;
}

/**
 * The workspace a git path names and the caller's right to it.
 *
 * The checks run in the order the contract fixes, and the order is the
 * behaviour: the workspace and its organization, then whether the
 * repository is ready, then access, then the token's git scopes. A
 * member's push is therefore refused for being a member whatever the
 * token holds, and a workspace nobody may see is indistinguishable from
 * one that was never there.
 */
export async function gitAccess(
  c: Context,
  deps: GitAccessDeps,
  need: 'read' | 'write',
): Promise<GitSubject> {
  const repo = c.req.param('repo') ?? '';
  if (!repo.endsWith('.git')) {
    throw new GitRefusal(404, NOT_FOUND);
  }

  const identity = await identifyCredential(asBearer(c.req.header('authorization')), {
    store: deps.store,
    clock: deps.clock,
    mountPoint: '',
    ...(deps.adminToken === undefined ? {} : { adminToken: deps.adminToken }),
  });
  if (identity === undefined) {
    // The challenge is what makes a git client send the credential it
    // holds: without it git reports the refusal as final.
    throw new GitRefusal(401, UNAUTHENTICATED, { 'www-authenticate': 'Basic realm="Parley"' });
  }

  const slug = repo.slice(0, -'.git'.length);
  const workspace = await findWorkspace(deps.store, slug);
  const org =
    workspace === undefined || workspace.org_id === null
      ? undefined
      : await findOrgByNameOrSlug(deps.store, '', c.req.param('org') ?? '');

  if (workspace === undefined || org === undefined || org.id !== workspace.org_id) {
    throw new GitRefusal(404, NOT_FOUND);
  }
  // A token bound to another workspace is told the same as a stranger.
  if (identity.boundTo !== undefined && identity.boundTo !== workspace.slug) {
    throw new GitRefusal(404, NOT_FOUND);
  }
  if (workspace.status !== 'ready') {
    throw new GitRefusal(409, NOT_AVAILABLE);
  }

  const level = await accessLevel(deps.store, identity, workspace);
  if (level === 'member' && need === 'write') {
    throw new GitRefusal(403, MEMBERS_READ_ONLY);
  }
  if (level === undefined) {
    throw new GitRefusal(404, NOT_FOUND);
  }

  const scope = need === 'write' ? 'git:write' : 'git:read';
  if (identity.kind === 'pat' && !grantsScope(identity.permissions, scope)) {
    throw new GitRefusal(403, `this token does not hold ${scope}`);
  }

  if (deps.repos === undefined) {
    throw new GitRefusal(503, NOT_AVAILABLE);
  }
  return {
    workspace,
    repository: repositoryOf(workspace),
    repos: deps.repos,
    identity,
  };
}
