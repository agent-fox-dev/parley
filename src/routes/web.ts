import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { AppHolder, RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { ProviderDirectory } from '../auth/oauth.js';
import { apiReader } from '../../web/api_client.js';
import {
  renderDashboard,
  renderLanding,
  renderNotAvailable,
  renderWorkspace,
} from '../../web/render.js';
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
  /** Where the API is served, which is what every page reads. */
  mountPoint: string;
  /** Where the app appears once it is built; the pages read through it. */
  holder?: AppHolder;
}

/**
 * The reader a page uses: the service's own API, asked as the
 * person signed in. The session value is their credential, so a
 * page is given exactly what their own key would be given.
 */
export function readerFor(config: WebRouteConfig, session: string) {
  const app = config.holder?.app;
  if (app === undefined) {
    throw new HttpError(500, 'the interface has no API to read');
  }
  return apiReader({ app: app as never, mountPoint: config.mountPoint, credential: session });
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

/**
 * One workspace, or the one answer every workspace a person cannot
 * open shares. The page itself is task 6's; what is settled here is
 * who may see one at all, and that is the API's decision rather
 * than a second rule of the interface's own.
 */
async function workspacePage(c: Context, config: WebRouteConfig): Promise<Response> {
  const who = await viewer(c, config);
  if (who === undefined) {
    return landing(c, config);
  }

  const session = sessionFrom(c.req.raw.headers) ?? '';
  const read = await readerFor(config, session).read<Record<string, unknown>>(
    `/workspaces/${encodeURIComponent(c.req.param('slug') ?? '')}`,
  );
  if (!read.ok) {
    // A workspace of somebody else's, one they may not see and an
    // address naming nothing are one answer: anything else would
    // let a person learn which slugs exist by trying them.
    return page(renderNotAvailable(who.username), 404);
  }
  return page(renderWorkspace(who.username, read.value));
}

async function dashboardPage(c: Context, config: WebRouteConfig): Promise<Response> {
  const who = await viewer(c, config);
  if (who === undefined) {
    return landing(c, config);
  }

  const session = sessionFrom(c.req.raw.headers) ?? '';
  const read = await readerFor(config, session).read<Array<Record<string, unknown>>>('/workspaces');
  return page(renderDashboard(who.username, read.ok ? read.value : undefined));
}

export function webRoutes(config: WebRouteConfig): RouteDefinition[] {
  return [
    { path: '/', handler: (c) => landing(c, config) },
    { path: '/dashboard', handler: async (c) => dashboardPage(c, config) },
    { path: '/workspaces/:slug', handler: async (c) => workspacePage(c, config) },
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
