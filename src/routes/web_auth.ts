import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import { endSession, openSession, sessionCookie, sessionFrom } from '../../web/session.js';
import { signedInUser } from './auth.js';

/**
 * Signing a person in through the browser.
 *
 * It uses the same provider the API uses and finds the same person,
 * and it deliberately does **not** go through
 * `POST /api/v1/auth/callback`: that route revokes every API key the
 * person holds, so a developer opening the dashboard would silently
 * kill the key their agent was running with. This issues no key and
 * revokes none.
 */
export interface WebAuthConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  providers?: ProviderDirectory;
}

/** Where the browser is sent once it has a session, or has lost one. */
const AFTER_SIGN_IN = '/dashboard';
const AFTER_SIGN_OUT = '/';

/** Whether the request came in over TLS, which decides the cookie's Secure. */
function overTls(c: Context): boolean {
  const url = new URL(c.req.url);
  return url.protocol === 'https:' || c.req.header('x-forwarded-proto') === 'https';
}

function seeOther(location: string, cookie?: string): Response {
  return new Response(null, {
    status: 302,
    headers: { location, ...(cookie === undefined ? {} : { 'set-cookie': cookie }) },
  });
}

async function begin(c: Context, config: WebAuthConfig): Promise<Response> {
  const provider = config.providers?.find(c.req.param('provider') ?? '');
  if (provider === undefined) {
    throw new HttpError(404, 'that provider is not configured');
  }
  // The provider's own authorize URL, with this deployment's
  // callback as the place to come back to.
  const back = new URL('/signin/' + provider.name + '/callback', c.req.url).href;
  const sent = new URL(provider.authorizeUrl);
  sent.searchParams.set('redirect_uri', back);
  return Promise.resolve(seeOther(sent.href));
}

async function complete(c: Context, config: WebAuthConfig): Promise<Response> {
  const provider = config.providers?.find(c.req.param('provider') ?? '');
  if (provider === undefined) {
    throw new HttpError(404, 'that provider is not configured');
  }
  const code = new URL(c.req.url).searchParams.get('code') ?? '';
  if (code.length === 0) {
    throw new HttpError(400, 'the provider sent no authorization code');
  }

  // The same exchange and the same person the API would find — and
  // nothing else: no key is issued and none is revoked.
  const identity = await provider.exchange(code);
  const user = await signedInUser({ store: config.store }, identity, config.clock.now());

  const session = await openSession(config.store, config.clock, user.id);
  return seeOther(AFTER_SIGN_IN, sessionCookie(session.value, { secure: overTls(c) }));
}

async function finish(c: Context, config: WebAuthConfig): Promise<Response> {
  await endSession(config.store, sessionFrom(c.req.raw.headers));
  return seeOther(AFTER_SIGN_OUT, sessionCookie('', { secure: overTls(c), clear: true }));
}

export function webAuthRoutes(config: WebAuthConfig): RouteDefinition[] {
  return [
    { path: '/signin/:provider', handler: async (c) => begin(c, config) },
    { path: '/signin/:provider/callback', handler: async (c) => complete(c, config) },
    { path: '/signout', handler: async (c) => finish(c, config) },
  ];
}
