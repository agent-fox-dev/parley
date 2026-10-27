import type { Context, MiddlewareHandler } from 'hono';
import { HttpError, errorResponse } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { SERVER_ROUTE_PATHS } from '../routes/server.js';
import { findCredential, findUser } from '../store/identity.js';
import { bearerCredential, hashSecret, parseCredential, sameDigest } from './credentials.js';
import type { CredentialKind } from './credentials.js';
import { grantsScope } from './scopes.js';

/** Who is calling, as every route that runs after authentication sees it. */
export interface Identity {
  kind: CredentialKind;
  /** Absent for an admin token, which has no user of its own. */
  userId?: string;
  credentialId?: string;
  /** The scopes a token was granted. Empty for the kinds that hold them all. */
  permissions: readonly string[];
}

export interface AuthOptions {
  store: RelationalStore;
  clock: Clock;
  /** The deployment's admin token, where one is configured. */
  adminToken?: string;
  mountPoint: string;
  /** Where each answered request is recorded, with no credential in it. */
  log?: LogSink;
}

type IdentityContext = Context<{ Variables: { identity: Identity } }>;

const UNAUTHENTICATED = 'authentication required';

/** The routes served to a caller who presents nothing. */
export function isPublicPath(path: string, mountPoint: string): boolean {
  return (
    (SERVER_ROUTE_PATHS as readonly string[]).includes(path) ||
    path === `${mountPoint}/auth/providers` ||
    path === `${mountPoint}/auth/callback` ||
    path.startsWith('/hooks/forge/')
  );
}

/**
 * Identifies the caller, or answers 401. Every refusal gives the same
 * answer, so a caller cannot learn from it whether the credential was
 * unknown, malformed, revoked, expired or that of a blocked user.
 */
export function authentication(options: AuthOptions): MiddlewareHandler {
  return async (c, next) => {
    if (!isPublicPath(new URL(c.req.url).pathname, options.mountPoint)) {
      const identity = await identify(c.req.header('authorization'), options);
      if (identity === undefined) {
        record(options.log, c, 401);
        return errorResponse(new HttpError(401, UNAUTHENTICATED));
      }
      (c as IdentityContext).set('identity', identity);
    }

    await next();
    record(options.log, c, c.res.status);
    return undefined;
  };
}

export function identityOf(c: Context): Identity | undefined {
  return (c as IdentityContext).get('identity');
}

/**
 * The answer for a personal access token that does not hold the scope, or
 * undefined when the caller may proceed. The kinds that hold every scope
 * are admitted without a list to consult.
 *
 * `hidden` is the anti-enumeration answer: the refusal is dressed as a
 * workspace that does not exist, so the route discloses nothing.
 */
export function scopeRefusal(c: Context, scope: string, hidden = false): Response | undefined {
  const identity = identityOf(c);
  if (identity?.kind !== 'pat' || grantsScope(identity.permissions, scope)) {
    return undefined;
  }
  return errorResponse(
    hidden
      ? new HttpError(404, 'workspace not found')
      : new HttpError(403, `this token does not hold ${scope}`),
  );
}

async function identify(
  header: string | undefined,
  options: AuthOptions,
): Promise<Identity | undefined> {
  const presented = bearerCredential(header);
  if (presented === undefined) {
    return undefined;
  }

  if (options.adminToken !== undefined && sameDigest(presented, options.adminToken)) {
    return { kind: 'admin_token', permissions: [] };
  }

  const parsed = parseCredential(presented);
  if (parsed === undefined) {
    return undefined;
  }

  const credential = await findCredential(options.store, parsed.kind, parsed.id);
  if (credential === undefined || credential.revoked_at !== null) {
    return undefined;
  }
  if (
    credential.expires_at !== null &&
    Date.parse(credential.expires_at) <= options.clock.now().getTime()
  ) {
    return undefined;
  }
  if (!sameDigest(await hashSecret(parsed.secret), credential.secret_hash)) {
    return undefined;
  }

  // Read on every request, never cached: a block has to take effect at once.
  const user = await findUser(options.store, credential.user_id);
  if (user === undefined || user.status === 'blocked') {
    return undefined;
  }

  return {
    kind: parsed.kind,
    userId: user.id,
    credentialId: credential.id,
    permissions: credential.permissions,
  };
}

function record(log: LogSink | undefined, c: Context, status: number): void {
  log?.info?.('answered a request', {
    method: c.req.method,
    route: c.req.routePath === '/*' ? new URL(c.req.url).pathname : c.req.routePath,
    status,
  });
}
