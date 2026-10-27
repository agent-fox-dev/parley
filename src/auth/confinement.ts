import type { Context, MiddlewareHandler } from 'hono';
import { HttpError, errorResponse } from '../http/errors.js';
import { identityOf } from './middleware.js';

const NOT_FOUND = 'workspace not found';
const BOUND = 'token_bound';

/**
 * The identity routes. A bound token reaching one is refused without its
 * workspace being named, because the reference refuses it there before it
 * is authenticated and so cannot know which workspace that is.
 */
function isIdentityRoute(rest: string): boolean {
  return (
    rest === '/user' ||
    rest.startsWith('/user/') ||
    rest === '/users' ||
    rest.startsWith('/users/') ||
    rest === '/orgs' ||
    rest.startsWith('/orgs/') ||
    rest.startsWith('/auth/')
  );
}

/**
 * What a token bound to one workspace is told on the path it asked for, or
 * undefined where it may proceed.
 *
 * A route naming another workspace is answered as though that workspace did
 * not exist, before anything else is checked, so the token learns neither
 * whether it exists nor what state it is in.
 */
export function confinementRefusal(
  path: string,
  mountPoint: string,
  method: string,
  boundTo: string,
): HttpError | undefined {
  if (!path.startsWith(`${mountPoint}/`)) {
    return undefined;
  }
  const rest = path.slice(mountPoint.length);

  if (rest.startsWith('/workspaces/')) {
    const slug = rest.slice('/workspaces/'.length).split('/')[0] ?? '';
    return slug === boundTo ? undefined : new HttpError(404, NOT_FOUND);
  }
  // The list is its own workspace's, which the route narrows for it.
  if (rest === '/workspaces' && method === 'GET') {
    return undefined;
  }
  // Every caller sees the same profiles, so a bound token may read them.
  if (rest === '/sandbox-profiles') {
    return undefined;
  }
  if (isIdentityRoute(rest)) {
    return new HttpError(403, 'this token is bound to a workspace', BOUND);
  }
  return new HttpError(403, `this token is bound to workspace ${boundTo}`, BOUND);
}

/** Confines a bound token to its workspace, ahead of every other check. */
export function confinement(mountPoint: string): MiddlewareHandler {
  return async (c, next) => {
    const boundTo = identityOf(c)?.boundTo;
    if (boundTo !== undefined) {
      const refusal = confinementRefusal(
        new URL(c.req.url).pathname,
        mountPoint,
        c.req.method,
        boundTo,
      );
      if (refusal !== undefined) {
        return errorResponse(refusal);
      }
    }
    await next();
    return undefined;
  };
}

/** The workspace the caller's token is bound to, for a route that narrows by it. */
export function boundWorkspaceOf(c: Context): string | undefined {
  return identityOf(c)?.boundTo;
}
