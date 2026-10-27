import { HttpError, errorResponse } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';

/**
 * Every sandbox path the API reference lists. They are registered rather
 * than left out: an unrouted path answers 404 with no error_type, which is
 * a different answer from the one a deployment without sandboxes owes, and
 * `afc sandbox` tells the two apart.
 */
export const SANDBOX_PATHS: ReadonlyArray<{ path: string; methods: readonly string[] }> = [
  { path: '/sandbox-profiles', methods: ['GET'] },
  { path: '/workspaces/:slug/sandboxes', methods: ['POST'] },
  { path: '/workspaces/:slug/sandboxes', methods: ['GET'] },
  { path: '/workspaces/:slug/sandboxes/:id', methods: ['GET'] },
  { path: '/workspaces/:slug/sandboxes/:id', methods: ['DELETE'] },
  { path: '/sandboxes', methods: ['GET'] },
];

const DISABLED = 'sandboxes are not enabled on this hub';

/**
 * The sandbox routes of a deployment with no provider configured. Each
 * answers before any scope is checked and before the workspace is looked
 * up, so the answer says nothing about either.
 */
export function sandboxRoutes(mountPoint: string): RouteDefinition[] {
  return SANDBOX_PATHS.map((route) => ({
    path: `${mountPoint}${route.path}`,
    methods: route.methods,
    handler: () => errorResponse(new HttpError(404, DISABLED, 'sandboxes_disabled')),
  }));
}
