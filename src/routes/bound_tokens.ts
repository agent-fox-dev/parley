import type { Context } from 'hono';
import { identityOf } from '../auth/middleware.js';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { bindToken, boundTokensOf } from '../store/bindings.js';
import { revokeCredential } from '../store/identity.js';
import { workspaceAccess } from '../workspace/access.js';
import { mintToken, requireTokenScope, tokenMetadata } from './tokens.js';

export interface BoundTokenRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** A bound token can neither mint nor list bound tokens, whatever its scopes. */
function refuseBoundCaller(c: Context): void {
  if (identityOf(c)?.boundTo !== undefined) {
    throw new HttpError(403, 'this token is bound to a workspace', 'token_bound');
  }
}

async function mintBoundToken(c: Context, config: BoundTokenRouteConfig): Promise<Response> {
  refuseBoundCaller(c);
  const identity = identityOf(c);
  if (identity?.userId === undefined) {
    throw new HttpError(
      403,
      'admin tokens cannot mint bound tokens; a real user is required as the token owner',
    );
  }

  // Private access: a member of the organization is told the workspace is
  // not there, exactly as a stranger is.
  const { workspace } = await workspaceAccess(c, config.store, 'private');
  if (workspace.owner_id !== identity.userId) {
    throw new HttpError(404, 'workspace not found');
  }
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'workspace is archived');
  }

  requireTokenScope(c, 'tokens:manage');
  const minted = await mintToken(c, config.store, config.clock, identity.userId);

  try {
    await bindToken(
      config.store,
      minted.token.id,
      workspace.slug,
      config.clock.now().toISOString(),
    );
  } catch {
    // A token that could not be bound would be an unbound token nobody
    // asked for, so it does not survive the failure.
    await revokeCredential(config.store, minted.token.id, config.clock.now().toISOString());
    throw new HttpError(500, 'failed to bind the token to the workspace');
  }

  return jsonResponse(
    {
      ...tokenMetadata(minted.token),
      token: minted.presented,
      workspace: workspace.slug,
    },
    201,
    { 'cache-control': 'no-store' },
  );
}

async function listBoundTokens(c: Context, config: BoundTokenRouteConfig): Promise<Response> {
  refuseBoundCaller(c);
  requireTokenScope(c, 'tokens:read');
  // Private access, and it works on an archived workspace.
  const { workspace } = await workspaceAccess(c, config.store, 'private');

  const tokens = await boundTokensOf(config.store, workspace.slug);
  return jsonResponse(
    tokens.map((token) => ({ ...tokenMetadata(token), workspace: workspace.slug })),
  );
}

export function boundTokenRoutes(config: BoundTokenRouteConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/tokens`,
      methods: ['POST'],
      handler: async (c) => mintBoundToken(c, config),
    },
    {
      path: `${config.mountPoint}/workspaces/:slug/tokens`,
      handler: async (c) => listBoundTokens(c, config),
    },
  ];
}
