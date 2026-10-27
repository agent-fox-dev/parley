import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import { isMember } from '../store/identity.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import type { AccessLevel } from './serialise.js';

/** What a route needs of its caller towards the workspace it names. */
export type AccessNeed = 'read' | 'write' | 'private';

export interface WorkspaceAccess {
  workspace: WorkspaceRecord;
  level: AccessLevel;
}

const NOT_FOUND = 'workspace not found';
const MEMBERS_READ_ONLY = 'org members have read-only access to this workspace';

/**
 * The six routes that answer a personal access token lacking the scope as
 * though the workspace did not exist. Every other route, the create
 * included, answers the ordinary refusal. Route modules read this rather
 * than each deciding for itself.
 */
export const ANTI_ENUMERATION_ROUTES: ReadonlySet<string> = new Set([
  'GET /workspaces',
  'GET /workspaces/:slug',
  'PATCH /workspaces/:slug',
  'DELETE /workspaces/:slug',
  'POST /workspaces/:slug/archive',
  'POST /workspaces/:slug/reactivate',
]);

export function hidesMissingScope(method: string, path: string): boolean {
  return ANTI_ENUMERATION_ROUTES.has(`${method} ${path}`);
}

/**
 * The caller's level towards a workspace, or undefined when they have
 * none. Membership is read here, on every request, so a user removed from
 * the organization loses access with the next one.
 */
export async function accessLevel(
  store: RelationalStore,
  identity: ReturnType<typeof identityOf>,
  workspace: WorkspaceRecord,
): Promise<AccessLevel | undefined> {
  if (identity === undefined) {
    return undefined;
  }
  if (identity.kind === 'admin_token') {
    return 'admin';
  }
  if (identity.userId === undefined) {
    return undefined;
  }
  if (workspace.owner_id === identity.userId) {
    return 'owner';
  }
  if (workspace.org_id !== null && (await isMember(store, workspace.org_id, identity.userId))) {
    return 'member';
  }
  return undefined;
}

/**
 * The workspace a route names and the caller's level towards it, or the
 * refusal the access table gives. Every route that names a workspace comes
 * through here, so the three levels are decided once.
 *
 * A workspace nobody may see and a slug naming nothing give the same
 * answer, so no refusal says whether a workspace exists.
 */
export async function workspaceAccess(
  c: Context,
  store: RelationalStore,
  need: AccessNeed,
  slug = c.req.param('slug') ?? '',
): Promise<WorkspaceAccess> {
  const workspace = await findWorkspace(store, slug);
  if (workspace === undefined) {
    throw new HttpError(404, NOT_FOUND);
  }

  const level = await accessLevel(store, identityOf(c), workspace);
  if (level === 'admin' || level === 'owner') {
    return { workspace, level };
  }
  if (level === 'member' && need === 'read') {
    return { workspace, level };
  }
  if (level === 'member' && need === 'write') {
    throw new HttpError(403, MEMBERS_READ_ONLY, 'forbidden');
  }
  // A member on a private route, and a stranger anywhere, are told the
  // same thing as somebody asking after a workspace that never existed.
  throw new HttpError(404, NOT_FOUND);
}
