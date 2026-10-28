import { HttpError } from '../http/errors.js';
import type { ForgeKind } from './verify.js';

/**
 * What a delivery means, as the reference's two mapping tables give
 * it.
 *
 * Both tables are transcribed here and read from here: a mapping
 * spread across the code would drift from the reference one row at
 * a time, and a row that drifts is a consumer's filter that quietly
 * stops matching.
 *
 * Nothing in this module reaches the forge or keeps the payload. It
 * takes what one delivery says and answers with the events it owes.
 */
export interface MappedEvent {
  type: string;
  resourceId: string;
  /** Only the keys the event's own declaration lists. */
  metadata: Record<string, unknown>;
}

export interface MappedDelivery {
  /** The repository path the payload is about, for the target check. */
  repository: string;
  /** The payload's own action, for the log row. */
  action: string;
  events: MappedEvent[];
}

/** How much of a title is carried, and how many labels. */
const TITLE_BYTES = 256;
const MOST_LABELS = 100;

/** How many label events one delivery may relay. */
const MOST_LABEL_EVENTS = 100;

function object(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function cut(value: string, bytes: number): string {
  const encoded = new TextEncoder().encode(value);
  return encoded.length <= bytes
    ? value
    : new TextDecoder().decode(encoded.slice(0, bytes)).replace(/�$/, '');
}

/** A number the delivery is about, which it must have. */
function numbered(value: unknown, what: string): number {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  throw new HttpError(400, `the delivery has no ${what}`);
}

/** The names of labels, however the forge spells them. */
function labelNames(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value
    .map((label) => (typeof label === 'string' ? label : text(object(label)['name'])))
    .filter((name) => name.length > 0)
    .slice(0, MOST_LABELS);
}

export interface DeliveryContext {
  forge: ForgeKind;
  deliveryId: string;
  /** The login of whoever did it on the forge, where the payload says. */
  actor: string;
}

/** The metadata every delivery event carries before its own keys. */
function common(context: DeliveryContext): Record<string, unknown> {
  return { forge: context.forge, delivery_id: context.deliveryId, actor: context.actor };
}

/** GitHub's issues, which carry their own labels. */
function githubIssues(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  const action = text(payload['action']);
  const issue = object(payload['issue']);
  const number = numbered(issue['number'], 'issue number');
  const about = {
    ...common(context),
    number,
    title: cut(text(issue['title']), TITLE_BYTES),
    url: text(issue['html_url']),
  };

  if (['opened', 'edited', 'closed', 'reopened'].includes(action)) {
    return [
      {
        type: `hub.forge.issue.${action}`,
        resourceId: String(number),
        metadata: { ...about, labels: labelNames(issue['labels']) },
      },
    ];
  }
  if (action === 'labeled' || action === 'unlabeled') {
    const label = text(object(payload['label'])['name']);
    if (label.length === 0) {
      throw new HttpError(400, 'the delivery has no label name');
    }
    // A label change carries the label and the set it leaves
    // behind; the title belongs to the events about the issue.
    return [
      {
        type: `hub.forge.issue.${action}`,
        resourceId: String(number),
        metadata: {
          ...common(context),
          number,
          label,
          labels: labelNames(issue['labels']),
          url: text(issue['html_url']),
        },
      },
    ];
  }
  return [];
}

/** A comment, which GitHub reports on issues and pull requests alike. */
function githubComment(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  if (text(payload['action']) !== 'created') {
    return [];
  }
  const issue = object(payload['issue']);
  const number = numbered(issue['number'], 'issue number');
  return [
    {
      type: 'hub.forge.comment.created',
      resourceId: String(number),
      metadata: {
        ...common(context),
        number,
        comment_url: text(object(payload['comment'])['html_url']),
        // GitHub answers a pull request from the issue routes and
        // says so by carrying a pull_request block on the issue.
        pull_request: issue['pull_request'] !== undefined,
      },
    },
  ];
}

/** A pull request, whose close is a merge or not. */
function githubPull(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  const action = text(payload['action']);
  const pull = object(payload['pull_request']);
  const number = numbered(pull['number'], 'pull request number');
  const merged = pull['merged'] === true;

  const named =
    action === 'closed'
      ? merged
        ? 'merged'
        : 'closed'
      : ['opened', 'reopened', 'synchronize'].includes(action)
        ? action
        : undefined;
  if (named === undefined) {
    return [];
  }
  return [
    {
      type: `hub.forge.pull_request.${named}`,
      resourceId: String(number),
      metadata: {
        ...common(context),
        number,
        url: text(pull['html_url']),
        head: text(object(pull['head'])['ref']),
        base: text(object(pull['base'])['ref']),
        merged,
      },
    },
  ];
}

/** A push, which both forges report the same way. */
function pushed(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  const ref = text(payload['ref']);
  if (ref.length === 0) {
    throw new HttpError(400, 'the delivery has no ref');
  }
  return [
    {
      type: 'hub.forge.push',
      resourceId: ref,
      metadata: {
        ...common(context),
        ref,
        before: text(payload['before']),
        after: text(payload['after']),
      },
    },
  ];
}

/** GitHub's table. */
function fromGithub(
  event: string,
  payload: Record<string, unknown>,
  context: DeliveryContext,
): MappedEvent[] {
  if (event === 'issues') {
    return githubIssues(payload, context);
  }
  if (event === 'issue_comment') {
    return githubComment(payload, context);
  }
  if (event === 'pull_request') {
    return githubPull(payload, context);
  }
  return event === 'push' ? pushed(payload, context) : [];
}

/** What a GitLab update changed about an issue's labels. */
function labelChanges(changes: Record<string, unknown>): { removed: string[]; added: string[] } {
  const labels = object(changes['labels']);
  const before = labelNames(labels['previous']);
  const after = labelNames(labels['current']);
  return {
    removed: before.filter((name) => !after.includes(name)).toSorted(),
    added: after.filter((name) => !before.includes(name)).toSorted(),
  };
}

/** GitLab's issues, including the update that is several events. */
function gitlabIssue(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  const attributes = object(payload['object_attributes']);
  const action = text(attributes['action']);
  const number = numbered(attributes['iid'], 'issue number');
  const about = {
    ...common(context),
    number,
    title: cut(text(attributes['title']), TITLE_BYTES),
    url: text(attributes['url']),
  };

  const named = { open: 'opened', close: 'closed', reopen: 'reopened' }[action];
  if (named !== undefined) {
    return [
      {
        type: `hub.forge.issue.${named}`,
        resourceId: String(number),
        metadata: { ...about, labels: labelNames(payload['labels']) },
      },
    ];
  }
  if (action !== 'update') {
    return [];
  }

  const changes = object(payload['changes']);
  const events: MappedEvent[] = [];
  if (changes['title'] !== undefined || changes['description'] !== undefined) {
    events.push({
      type: 'hub.forge.issue.edited',
      resourceId: String(number),
      metadata: { ...about, labels: labelNames(payload['labels']) },
    });
  }

  // Removals before additions, each group sorted, and the whole lot
  // capped: a hundred label events is already more than any
  // consumer wanted from one delivery.
  const { removed, added } = labelChanges(changes);
  const labelEvents = [
    ...removed.map((label) => ['unlabeled', label] as const),
    ...added.map((label) => ['labeled', label] as const),
  ].slice(0, MOST_LABEL_EVENTS);

  for (const [what, label] of labelEvents) {
    events.push({
      type: `hub.forge.issue.${what}`,
      resourceId: String(number),
      metadata: {
        ...common(context),
        number,
        label,
        labels: labelNames(payload['labels']),
        url: text(attributes['url']),
      },
    });
  }
  return events;
}

/** A note, which is a comment only where it is on an issue or a merge request. */
function gitlabNote(payload: Record<string, unknown>, context: DeliveryContext): MappedEvent[] {
  const attributes = object(payload['object_attributes']);
  const action = text(attributes['action']);
  const noteable = text(attributes['noteable_type']);

  // A note on a commit or a snippet is nothing this relays.
  if (noteable !== 'Issue' && noteable !== 'MergeRequest') {
    return [];
  }
  if (action.length > 0 && action !== 'create') {
    return [];
  }

  const about = object(noteable === 'Issue' ? payload['issue'] : payload['merge_request']);
  const number = numbered(about['iid'], 'issue or merge request number');
  return [
    {
      type: 'hub.forge.comment.created',
      resourceId: String(number),
      metadata: {
        ...common(context),
        number,
        comment_url: text(attributes['url']),
        pull_request: noteable === 'MergeRequest',
      },
    },
  ];
}

/** A merge request, whose update is a synchronize only with a new revision. */
function gitlabMergeRequest(
  payload: Record<string, unknown>,
  context: DeliveryContext,
): MappedEvent[] {
  const attributes = object(payload['object_attributes']);
  const action = text(attributes['action']);
  const number = numbered(attributes['iid'], 'merge request number');

  const named =
    { open: 'opened', close: 'closed', reopen: 'reopened', merge: 'merged' }[action] ??
    (action === 'update' && attributes['oldrev'] !== undefined ? 'synchronize' : undefined);
  if (named === undefined) {
    return [];
  }
  return [
    {
      type: `hub.forge.pull_request.${named}`,
      resourceId: String(number),
      metadata: {
        ...common(context),
        number,
        url: text(attributes['url']),
        head: text(attributes['source_branch']),
        base: text(attributes['target_branch']),
        merged: named === 'merged',
      },
    },
  ];
}

/** GitLab's table. */
function fromGitlab(
  event: string,
  payload: Record<string, unknown>,
  context: DeliveryContext,
): MappedEvent[] {
  if (event === 'Issue Hook') {
    return gitlabIssue(payload, context);
  }
  if (event === 'Note Hook') {
    return gitlabNote(payload, context);
  }
  if (event === 'Merge Request Hook') {
    return gitlabMergeRequest(payload, context);
  }
  return event === 'Push Hook' || event === 'Tag Push Hook' ? pushed(payload, context) : [];
}

/** The repository a payload is about, which every delivery must name. */
function repositoryOf(forge: ForgeKind, payload: Record<string, unknown>): string {
  const named =
    forge === 'github'
      ? text(object(payload['repository'])['full_name'])
      : text(object(payload['project'])['path_with_namespace']);
  if (named.length === 0) {
    throw new HttpError(400, 'the delivery names no repository');
  }
  return named;
}

/** Whoever did it on the forge, where the payload says. */
function actorOf(forge: ForgeKind, payload: Record<string, unknown>): string {
  return forge === 'github'
    ? text(object(payload['sender'])['login'])
    : text(payload['user_username']) || text(object(payload['user'])['username']);
}

/**
 * What one delivery means. A body that is not one JSON object, or
 * that lacks what the delivery is about, is refused; a body missing
 * only a title, a URL or a branch is not, because those describe
 * the thing rather than identify it.
 */
export function mapDelivery(
  forge: ForgeKind,
  event: string,
  body: unknown,
  deliveryId: string,
): MappedDelivery {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    throw new HttpError(400, 'the delivery body is not one JSON object');
  }
  const payload = body as Record<string, unknown>;
  const repository = repositoryOf(forge, payload);
  const context: DeliveryContext = { forge, deliveryId, actor: actorOf(forge, payload) };

  const action =
    forge === 'github'
      ? text(payload['action'])
      : text(object(payload['object_attributes'])['action']);

  return {
    repository,
    action,
    events:
      forge === 'github'
        ? fromGithub(event, payload, context)
        : fromGitlab(event, payload, context),
  };
}
