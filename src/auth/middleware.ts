import type { Context, MiddlewareHandler } from 'hono';
import { HttpError, errorResponse } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { SERVER_ROUTE_PATHS } from '../routes/server.js';
import { bindingOf } from '../store/bindings.js';
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
  /** The workspace a bound token may reach, and no other. */
  boundTo?: string;
}

export interface AuthOptions {
  /**
   * Whose browser session a presented value is, where it is one.
   * The web interface reads the API as the person signed into it,
   * and this is how the credential it presents is recognised.
   */
  sessions?: (value: string) => Promise<string | undefined>;
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
/**
 * The git routes, which authenticate their own clients so that every
 * refusal reaches the client as pkt-line rather than as the envelope a
 * git client cannot read.
 */
/** The webhook ingress, where the forge's own secret is the credential. */
export function isIngressPath(path: string): boolean {
  return path.startsWith('/hooks/');
}

/**
 * The web interface, which a person reaches with a browser session
 * rather than a bearer credential. Its own routes decide what a
 * visitor without one is shown.
 */
export function isWebPath(path: string): boolean {
  return (
    path === '/' ||
    path === '/dashboard' ||
    path === '/signout' ||
    path.startsWith('/web/') ||
    path.startsWith('/signin') ||
    path.startsWith('/workspaces/')
  );
}

export function isGitPath(path: string): boolean {
  return path === '/git' || path.startsWith('/git/');
}

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
    const path = new URL(c.req.url).pathname;
    if (
      !isPublicPath(path, options.mountPoint) &&
      !isGitPath(path) &&
      !isIngressPath(path) &&
      !isWebPath(path)
    ) {
      const identity = await identifyCredential(c.req.header('authorization'), options);
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

/**
 * Who a credential belongs to, or nobody. The git routes identify their
 * own clients, in git's framing rather than the JSON envelope, so they
 * reach this directly rather than through the middleware.
 */
export async function identifyCredential(
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

  // A browser's session is a credential of the person signed in,
  // holding what their own key would hold and bound to nothing.
  const session = await options.sessions?.(presented);
  if (session !== undefined) {
    const person = await findUser(options.store, session);
    return person === undefined || person.status === 'blocked'
      ? undefined
      : { kind: 'web_session', userId: person.id, permissions: [] };
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

  // A binding that cannot be read must refuse the request, so this read is
  // deliberately not guarded: its failure becomes the 500 it should be.
  const boundTo = parsed.kind === 'pat' ? await bindingOf(options.store, credential.id) : undefined;

  return {
    kind: parsed.kind,
    userId: user.id,
    credentialId: credential.id,
    permissions: credential.permissions,
    ...(boundTo === undefined ? {} : { boundTo }),
  };
}

function record(log: LogSink | undefined, c: Context, status: number): void {
  log?.info?.('answered a request', {
    method: c.req.method,
    route: c.req.routePath === '/*' ? new URL(c.req.url).pathname : c.req.routePath,
    status,
  });
}
