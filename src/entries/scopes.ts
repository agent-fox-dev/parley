import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import { workspaceScope } from '../platform/secrets.js';
import { isMember } from '../store/identity.js';
import { type AccessNeed, workspaceAccess } from '../workspace/access.js';
import { orgScope, userScope } from './store.js';

export type ScopeFamily = 'user' | 'org' | 'workspace';

/**
 * The scope a request names, and who may reach it. The three families
 * differ only here; everything past this point treats them alike.
 *
 * - user: the caller's own, so an admin token, which has no user, is
 *   refused.
 * - org: by slug, for a member or an admin token; an organization that
 *   does not exist and one the caller is not in answer the same way.
 * - workspace: the access decision of the workspace package, at the level
 *   the kind needs.
 */
export async function entryScope(
  c: Context,
  store: RelationalStore,
  family: ScopeFamily,
  need: AccessNeed,
): Promise<string> {
  if (family === 'user') {
    const userId = identityOf(c)?.userId;
    if (userId === undefined) {
      throw new HttpError(403, 'this route is for a user credential; an admin token has no user');
    }
    return userScope(userId);
  }

  if (family === 'org') {
    const slug = c.req.param('slug') ?? '';
    const rows = await store.all<{ id: string }>('SELECT id FROM orgs WHERE slug = ?', [slug]);
    const orgId = rows[0]?.id;
    const identity = identityOf(c);
    if (orgId === undefined) {
      throw new HttpError(404, 'organization not found');
    }
    if (identity?.kind !== 'admin_token') {
      const userId = identity?.userId;
      if (userId === undefined || !(await isMember(store, orgId, userId))) {
        // A non-member is told what somebody asking after an organization
        // that does not exist is told.
        throw new HttpError(404, 'organization not found');
      }
    }
    return orgScope(orgId);
  }

  const { workspace } = await workspaceAccess(c, store, need);
  return workspaceScope(workspace.slug);
}
