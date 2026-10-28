import type { Context } from 'hono';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import { renderLanding } from '../../web/render.js';
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
