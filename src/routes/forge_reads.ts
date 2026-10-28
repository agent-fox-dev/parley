import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { checkedNumber, forgeGuard, reachedForge } from '../forge/guard.js';
import { githubApi } from '../forge/github.js';
import { gitlabApi } from '../forge/gitlab.js';
import type { Reached } from '../forge/client.js';
import type { ForgeApi, ForgeIssueThread, IssueQuery } from '../forge/types.js';
import { scrubbed } from '../forge/failures.js';
import { type ForgeRouteConfig, forgeDeps } from './forge.js';

/**
 * The nine reads. Each takes what the forge holds and answers with
 * the reference's shape; which forge answered is settled by the
 * mapper and visible nowhere above it.
 */
export type ForgeReadConfig = ForgeRouteConfig;

/** The forge's own client, as this service talks to it. */
export function apiFor(reached: Reached): ForgeApi {
  return reached.target.forge === 'github'
    ? githubApi(reached.client, reached.target.repository)
    : gitlabApi(reached.client, reached.target.repository);
}

/** One value of a query parameter, refused where it is given twice. */
function once(c: Context, name: string): string | undefined {
  const given = new URL(c.req.url).searchParams.getAll(name);
  if (given.length > 1) {
    throw new HttpError(400, `${name} must be given at most once`);
  }
  return given[0];
}

const STATES = ['open', 'closed', 'all'];
const SORTS = ['created', 'updated'];
const DIRECTIONS = ['asc', 'desc'];

/** How many issues one read brings back when the caller asks for none. */
const ISSUES_BY_DEFAULT = 30;

/**
 * What the caller asked an issue list for. A parameter they did not
 * give is left out entirely, so the forge's own default applies
 * rather than one this service invented.
 */
export function readIssueQuery(c: Context): IssueQuery {
  const named = (name: string, allowed: string[]): string | undefined => {
    const given = once(c, name);
    if (given === undefined) {
      return undefined;
    }
    if (!allowed.includes(given)) {
      throw new HttpError(400, `${name} must be one of: ${allowed.join(', ')}`);
    }
    return given;
  };

  const assignee = once(c, 'assignee');
  if (assignee !== undefined && assignee.length === 0) {
    throw new HttpError(400, 'assignee must not be empty');
  }

  const labels = once(c, 'labels')
    ?.split(',')
    .map((label) => label.trim())
    .filter((label) => label.length > 0);

  const asked = once(c, 'limit');
  let limit = ISSUES_BY_DEFAULT;
  if (asked !== undefined) {
    const wanted = Number(asked);
    if (!Number.isInteger(wanted) || wanted < 1 || wanted > 100) {
      throw new HttpError(400, 'limit must be an integer from 1 to 100');
    }
    limit = wanted;
  }

  return {
    ...(named('state', STATES) === undefined ? {} : { state: named('state', STATES) as string }),
    ...(labels === undefined || labels.length === 0 ? {} : { labels: labels.join(',') }),
    ...(assignee === undefined ? {} : { assignee }),
    ...(named('sort', SORTS) === undefined ? {} : { sort: named('sort', SORTS) as string }),
    ...(named('direction', DIRECTIONS) === undefined
      ? {}
      : { direction: named('direction', DIRECTIONS) as string }),
    limit,
  };
}

/** The forge a read works on, after every check the reference orders. */
async function reading(
  c: Context,
  config: ForgeReadConfig,
): Promise<{ api: ForgeApi; reached: Reached }> {
  const guarded = await forgeGuard(c, forgeDeps(config), 'read');
  const reached = await reachedForge(forgeDeps(config), guarded);
  return { api: apiFor(reached), reached };
}

/** The same, for a route whose path carries a number. */
async function readingNumbered(
  c: Context,
  config: ForgeReadConfig,
): Promise<{ api: ForgeApi; n: number }> {
  const guarded = await forgeGuard(c, forgeDeps(config), 'read');
  const n = checkedNumber(c.req.param('n') ?? '');
  const reached = await reachedForge(forgeDeps(config), guarded);
  return { api: apiFor(reached), n };
}

/**
 * An issue with its comments. The comments are the one part allowed
 * to fail on its own: an issue read whose comments were refused is
 * still an issue the caller asked for, so it is answered with the
 * comments' error rather than with none of it.
 */
async function thread(api: ForgeApi, n: number): Promise<ForgeIssueThread> {
  const issue = await api.issue(n);
  try {
    const read = await api.comments(n);
    return { issue, comments: read.comments, truncated: read.truncated };
  } catch (err) {
    return {
      issue,
      comments: [],
      truncated: false,
      comments_err: scrubbed(err instanceof Error ? err.message : 'the comments could not be read'),
    };
  }
}

export function forgeReadRoutes(config: ForgeReadConfig): RouteDefinition[] {
  const at = (path: string) => `${config.mountPoint}/workspaces/:slug/${path}`;
  const read = (path: string, handler: (c: Context) => Promise<Response>): RouteDefinition => ({
    path: at(path),
    scope: 'forge:read',
    handler,
  });

  return [
    read('forge/repository', async (c) =>
      jsonResponse(await (await reading(c, config)).api.repository()),
    ),
    read('issues', async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'read');
      const query = readIssueQuery(c);
      const reached = await reachedForge(forgeDeps(config), guarded);
      return jsonResponse(await apiFor(reached).issues(query));
    }),
    read('issues/:n', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse(await thread(api, n));
    }),
    read('issues/:n/comments', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse((await api.comments(n)).comments);
    }),
    read('pulls/:n', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse(await api.pull(n));
    }),
    read('pulls/:n/state', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      const pull = await api.pull(n);
      return jsonResponse({
        number: pull.number,
        state: pull.state,
        merged: pull.merged,
        head_sha: pull.head_sha,
      });
    }),
    read('pulls/:n/files', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse(await api.files(n));
    }),
    read('pulls/:n/checks', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse(await api.checks(n));
    }),
    read('pulls/:n/reviews', async (c) => {
      const { api, n } = await readingNumbered(c, config);
      return jsonResponse(await api.reviews(n));
    }),
  ];
}
