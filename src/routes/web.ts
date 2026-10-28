import type { Context } from 'hono';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import { renderLanding } from '../../web/render.js';
import { sessionFrom, signedInUserId } from '../../web/session.js';
import { findUser } from '../store/identity.js';
import { TOKENS } from '../../web/tokens.js';

/**
 * The web interface: the same service, rendered for a person.
 *
 * Every page is a server-rendered component and every read goes
 * through the service's own API as the signed-in person, so the
 * interface is one more client rather than a second way into the
 * store.
 */
export interface WebRouteConfig {
  store: RelationalStore;
  clock: Clock;
  providers?: ProviderDirectory;
  /** Where the pages are served from, for the links they write. */
  mountPoint: string;
}

/** An HTML answer, which is what every page of the interface is. */
export function page(body: string, status = 200): Response {
  return new Response(body, {
    status,
    headers: { 'content-type': 'text/html; charset=UTF-8' },
  });
}

/**
 * Who is reading, where anybody is. A session that has expired or
 * been signed out is nobody, which is what makes the landing page
 * the answer rather than an error.
 */
export async function viewer(
  c: Context,
  config: WebRouteConfig,
): Promise<{ id: string; username: string } | undefined> {
  // A request another site made carries the cookie but is not the
  // person: a browser says so, and the interface takes its word.
  if (c.req.header('sec-fetch-site') === 'cross-site') {
    return undefined;
  }
  const userId = await signedInUserId(config.store, config.clock, sessionFrom(c.req.raw.headers));
  if (userId === undefined) {
    return undefined;
  }
  const user = await findUser(config.store, userId);
  return user === undefined ? undefined : { id: user.id, username: user.username };
}

function landing(c: Context, config: WebRouteConfig): Response {
  void c;
  const named = config.providers === undefined ? [] : config.providers.list();
  return page(renderLanding(named.map((provider) => ({ name: provider.name }))));
}

export function webRoutes(config: WebRouteConfig): RouteDefinition[] {
  return [
    // Every page of the interface, until a session exists, is the
    // landing page: a visitor who guesses an address learns nothing
    // from the guess.
    ...['/', '/dashboard', '/workspaces/:slug'].map((path) => ({
      path,
      handler: (c: Context) => landing(c, config),
    })),
    {
      path: '/web/tokens.css',
      handler: () =>
        new Response(TOKENS, {
          headers: {
            'content-type': 'text/css; charset=UTF-8',
            'cache-control': 'public, max-age=300',
          },
        }),
    },
  ];
}
