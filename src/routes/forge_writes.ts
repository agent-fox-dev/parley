import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { type EventRecorder, actorIn, record } from '../audit/recorder.js';
import { checkedNumber, forgeGuard, reachedForge } from '../forge/guard.js';
import type { Reached } from '../forge/client.js';
import {
  type ForgeApi,
  type ForgeLabel,
  type IssueChanges,
  type NewIssue,
  type NewPullRequest,
  isNotFound,
} from '../forge/types.js';
import { type ForgeRouteConfig, forgeDeps } from './forge.js';
import { apiFor } from './forge_reads.js';

/**
 * The nine writes.
 *
 * Three of them make more than one request to the forge, and for
 * those the ordering is the contract: a close posts its comment
 * first and closes only if that worked, a label removal reads the
 * issue when the forge says not found, and a label create reads
 * again when its create was refused. What happened and in which
 * order is what the caller is answered and what the event records.
 *
 * Every write that reached the forge records exactly one event,
 * successful or not; one refused before it reached the forge records
 * none.
 */
export type ForgeWriteConfig = ForgeRouteConfig;

/** How many names a label or assignee list may hold. */
const MOST_NAMES = 100;

function body(raw: unknown): Record<string, unknown> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  return raw as Record<string, unknown>;
}

/** A field that must be a string with something in it. */
function required(held: Record<string, unknown>, name: string): string {
  const value = held[name];
  if (typeof value !== 'string' || value.trim().length === 0) {
    throw new HttpError(400, `${name} is required and must not be blank`);
  }
  return value;
}

function optionalText(held: Record<string, unknown>, name: string): string | undefined {
  const value = held[name];
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== 'string') {
    throw new HttpError(400, `${name} must be a string`);
  }
  return value;
}

/** A list of names, each with something in it and none too many. */
function names(value: unknown, what: string): string[] {
  if (!Array.isArray(value)) {
    throw new HttpError(400, `${what} must be an array of names`);
  }
  if (value.length > MOST_NAMES) {
    throw new HttpError(400, `${what} holds at most ${String(MOST_NAMES)} names`);
  }
  return value.map((name) => {
    if (typeof name !== 'string' || name.trim().length === 0) {
      throw new HttpError(400, `${what} must hold names, none of them blank`);
    }
    return name;
  });
}

/** The event each write records, by the route that made it. */
interface WriteEvent {
  type: string;
  resourceId: string;
  metadata: Record<string, unknown>;
}

/**
 * Makes a write and records exactly one event for it. The event is
 * written whether the forge took the change or refused it, because
 * the reference requires an operator to be able to see that the hub
 * tried; a failure before the forge never reaches here.
 */
async function writing<T>(
  c: Context,
  config: ForgeWriteConfig,
  reached: Reached,
  event: (outcome: { ok: boolean; result?: T; error?: HttpError }) => WriteEvent,
  work: () => Promise<T>,
): Promise<T> {
  const recorder: EventRecorder | undefined = config.events;
  const slug = c.req.param('slug') ?? '';

  const written = async (outcome: { ok: boolean; result?: T; error?: HttpError }) => {
    const said = event(outcome);
    await record(
      recorder,
      {
        type: said.type,
        workspace: slug,
        resourceId: said.resourceId,
        actor: actorIn(c),
        ...(outcome.ok ? {} : { severity: 'warning' as const }),
        metadata: {
          target: reached.target === undefined ? 'origin' : undefined,
          ...said.metadata,
          ok: outcome.ok,
          ...(outcome.error?.errorType === undefined
            ? {}
            : { error_type: outcome.error.errorType }),
        },
      },
      config.log,
    );
  };

  try {
    const result = await work();
    await written({ ok: true, result });
    return result;
  } catch (err) {
    await written({ ok: false, ...(err instanceof HttpError ? { error: err } : {}) });
    throw err;
  }
}

/** Everything a write needs, in the order the reference checks it. */
async function writeOn(
  c: Context,
  config: ForgeWriteConfig,
): Promise<{ api: ForgeApi; reached: Reached; target: string }> {
  const guarded = await forgeGuard(c, forgeDeps(config), 'write');
  const reached = await reachedForge(forgeDeps(config), guarded);
  return { api: apiFor(reached), reached, target: guarded.target };
}

export function forgeWriteRoutes(config: ForgeWriteConfig): RouteDefinition[] {
  const at = (path: string) => `${config.mountPoint}/workspaces/:slug/${path}`;
  const write = (
    path: string,
    methods: string[],
    handler: (c: Context) => Promise<Response>,
  ): RouteDefinition => ({ path: at(path), methods, scope: 'forge:write', handler });

  return [
    write('issues', ['POST'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const asked = body(await c.req.json().catch(() => undefined));
      const issue: NewIssue = {
        title: required(asked, 'title'),
        ...(optionalText(asked, 'body') === undefined
          ? {}
          : { body: optionalText(asked, 'body') as string }),
        ...(asked['labels'] === undefined ? {} : { labels: names(asked['labels'], 'labels') }),
        ...(asked['assignees'] === undefined
          ? {}
          : { assignees: names(asked['assignees'], 'assignees') }),
      };
      const reached = await reachedForge(forgeDeps(config), guarded);

      const created = await writing(
        c,
        config,
        reached,
        ({ ok, result }) => ({
          type: 'hub.forge.issue.create',
          resourceId: ok && result !== undefined ? String(result.number) : '',
          metadata: {
            target: guarded.target,
            ...(ok && result !== undefined ? { number: result.number, url: result.html_url } : {}),
          },
        }),
        async () => apiFor(reached).createIssue(issue),
      );
      return jsonResponse(created, 201);
    }),

    write('issues/:n', ['PATCH'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const n = checkedNumber(c.req.param('n') ?? '');
      const asked = body(await c.req.json().catch(() => undefined));

      const changes: IssueChanges = {};
      if (asked['title'] !== undefined && asked['title'] !== null) {
        changes.title = required(asked, 'title');
      }
      const changedBody = optionalText(asked, 'body');
      if (changedBody !== undefined) {
        changes.body = changedBody;
      }
      if (asked['state'] !== undefined && asked['state'] !== null) {
        const state = required(asked, 'state');
        if (state !== 'open' && state !== 'closed') {
          throw new HttpError(400, 'state must be open or closed');
        }
        changes.state = state;
      }
      if (asked['labels'] !== undefined && asked['labels'] !== null) {
        changes.labels = names(asked['labels'], 'labels');
      }
      if (asked['assignees'] !== undefined && asked['assignees'] !== null) {
        changes.assignees = names(asked['assignees'], 'assignees');
      }

      const reached = await reachedForge(forgeDeps(config), guarded);
      // GitLab takes user ids rather than names, so an update that
      // names only assignees would change nothing there.
      const changeable =
        reached.target.forge === 'gitlab'
          ? Object.keys(changes).filter((field) => field !== 'assignees')
          : Object.keys(changes);
      if (changeable.length === 0) {
        throw new HttpError(400, 'nothing to update');
      }

      const updated = await writing(
        c,
        config,
        reached,
        ({ ok, result }) => ({
          type: 'hub.forge.issue.update',
          resourceId: String(n),
          metadata: {
            target: guarded.target,
            number: n,
            fields: ok && result !== undefined ? result.fields : changeable.toSorted(),
          },
        }),
        async () => apiFor(reached).updateIssue(n, changes),
      );
      return jsonResponse(updated.issue);
    }),

    write('issues/:n/close', ['POST'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const n = checkedNumber(c.req.param('n') ?? '');
      const raw: unknown = await c.req.json().catch(() => undefined);
      const asked = raw === undefined ? {} : body(raw);
      const comment = optionalText(asked, 'comment') ?? '';

      const reached = await reachedForge(forgeDeps(config), guarded);
      const api = apiFor(reached);

      // The comment first, and the close only if it worked: a close
      // that happened without the comment it was asked to carry
      // cannot be undone.
      let commentUrl: string | undefined;
      const closed = await writing(
        c,
        config,
        reached,
        ({ ok }) => ({
          type: 'hub.forge.issue.close',
          resourceId: String(n),
          metadata: {
            target: guarded.target,
            number: n,
            ...(commentUrl === undefined ? {} : { comment_url: commentUrl }),
            ...(ok ? {} : {}),
          },
        }),
        async () => {
          if (comment.trim().length > 0) {
            commentUrl = (await api.commentOnIssue(n, comment)).url;
          }
          return api.closeIssue(n);
        },
      );
      return jsonResponse(closed);
    }),

    write('issues/:n/comments', ['POST'], async (c) => {
      const { api, reached, target } = await commentOn(c, config);
      const n = checkedNumber(c.req.param('n') ?? '');
      const said = required(body(await c.req.json().catch(() => undefined)), 'body');

      const posted = await writing(
        c,
        config,
        reached,
        ({ ok, result }) => ({
          type: 'hub.forge.comment.create',
          resourceId: String(n),
          metadata: {
            target,
            number: n,
            ...(ok && result !== undefined ? { url: result.url } : {}),
          },
        }),
        async () => api.commentOnIssue(n, said),
      );
      return jsonResponse(posted, 201);
    }),

    write('pulls/:n/comments', ['POST'], async (c) => {
      const { api, reached, target } = await commentOn(c, config);
      const n = checkedNumber(c.req.param('n') ?? '');
      const said = required(body(await c.req.json().catch(() => undefined)), 'body');

      const posted = await writing(
        c,
        config,
        reached,
        ({ ok, result }) => ({
          type: 'hub.forge.pull_request.comment',
          resourceId: String(n),
          metadata: {
            target,
            number: n,
            ...(ok && result !== undefined ? { url: result.url } : {}),
          },
        }),
        async () => api.commentOnPull(n, said),
      );
      return jsonResponse(posted, 201);
    }),

    write('issues/:n/labels', ['POST'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const n = checkedNumber(c.req.param('n') ?? '');
      const asked: unknown = await c.req.json().catch(() => undefined);
      const wanted = names(asked, 'labels');
      if (wanted.length === 0) {
        throw new HttpError(400, 'labels must hold at least one name');
      }

      const reached = await reachedForge(forgeDeps(config), guarded);
      await writing(
        c,
        config,
        reached,
        () => ({
          type: 'hub.forge.label.add',
          resourceId: String(n),
          metadata: { target: guarded.target, number: n, labels: wanted },
        }),
        async () => apiFor(reached).addLabels(n, wanted),
      );
      return emptyResponse(204);
    }),

    write('issues/:n/labels/:name', ['DELETE'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const n = checkedNumber(c.req.param('n') ?? '');
      // The last path segment, decoded exactly once: a label named
      // `type%2Fbug` is `type/bug`, and `af:fix` is itself.
      const name = c.req.param('name') ?? '';
      if (name.length === 0) {
        throw new HttpError(400, 'the label name must not be empty');
      }

      const reached = await reachedForge(forgeDeps(config), guarded);
      const api = apiFor(reached);

      await writing(
        c,
        config,
        reached,
        () => ({
          type: 'hub.forge.label.remove',
          resourceId: String(n),
          metadata: { target: guarded.target, number: n, label: name },
        }),
        async () => {
          try {
            await api.removeLabel(n, name);
          } catch (err) {
            if (!isNotFound(err)) {
              throw err;
            }
            // The forge says not found, which may be the label or
            // the issue. The issue decides: a label the issue never
            // had is not a failure.
            await api.issue(n);
          }
        },
      );
      return emptyResponse(204);
    }),

    write('labels', ['POST'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const asked = body(await c.req.json().catch(() => undefined));
      const label: ForgeLabel = {
        name: required(asked, 'name'),
        ...(asked['color'] === undefined || asked['color'] === null
          ? {}
          : { color: checkedColour(required(asked, 'color')) }),
        ...(optionalText(asked, 'description') === undefined
          ? {}
          : { description: optionalText(asked, 'description') as string }),
      };

      const reached = await reachedForge(forgeDeps(config), guarded);
      const api = apiFor(reached);

      let created = false;
      const held = await writing(
        c,
        config,
        reached,
        ({ ok }) => ({
          type: 'hub.forge.label.create',
          resourceId: label.name,
          metadata: {
            target: guarded.target,
            label: label.name,
            ...(ok ? { created } : {}),
          },
        }),
        async () => {
          // A label that exists is never changed: ensuring one twice
          // has to be harmless.
          const existing = await api.label(label.name);
          if (existing !== undefined) {
            return existing;
          }
          try {
            created = true;
            return await api.createLabel(label);
          } catch (err) {
            // A create refused may be a concurrent call that won the
            // race, so the repository is read once more.
            created = false;
            const now = await api.label(label.name);
            if (now === undefined) {
              throw err;
            }
            return now;
          }
        },
      );
      return jsonResponse(held, created ? 201 : 200);
    }),

    write('pulls', ['POST'], async (c) => {
      const guarded = await forgeGuard(c, forgeDeps(config), 'write');
      const asked = body(await c.req.json().catch(() => undefined));
      if (asked['draft'] !== undefined && typeof asked['draft'] !== 'boolean') {
        throw new HttpError(400, 'draft must be a boolean');
      }
      const pull: NewPullRequest = {
        title: required(asked, 'title'),
        head: required(asked, 'head'),
        base: required(asked, 'base'),
        ...(optionalText(asked, 'body') === undefined
          ? {}
          : { body: optionalText(asked, 'body') as string }),
        draft: asked['draft'] === true,
      };

      const reached = await reachedForge(forgeDeps(config), guarded);
      const opened = await writing(
        c,
        config,
        reached,
        ({ ok, result }) => ({
          type: 'hub.forge.pull_request.create',
          resourceId: ok && result !== undefined ? String(result.number) : '',
          metadata: {
            target: guarded.target,
            head: pull.head,
            base: pull.base,
            draft: pull.draft,
            ...(ok && result !== undefined ? { number: result.number, url: result.html_url } : {}),
          },
        }),
        async () => apiFor(reached).createPull(pull),
      );
      return jsonResponse(opened, 201);
    }),
  ];
}

/** Six hex digits, with or without the hash a forge may want. */
function checkedColour(given: string): string {
  const bare = given.replace(/^#/, '');
  if (!/^[0-9a-f]{6}$/i.test(bare)) {
    throw new HttpError(400, 'color must be six hexadecimal digits');
  }
  return bare;
}

/** What the two comment routes share. */
async function commentOn(c: Context, config: ForgeWriteConfig) {
  return writeOn(c, config);
}
