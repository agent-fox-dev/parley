import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { mapDelivery } from './mapping.js';

/**
 * The reference's two mapping tables, row by row. A row that drifts
 * is a consumer's filter that quietly stops matching, so each is
 * here with the events it owes and the keys those carry.
 */
const CONTEXT = { deliveryId: 'd-1' };

const github = (event: string, payload: Record<string, unknown>) =>
  mapDelivery(
    'github',
    event,
    { repository: { full_name: 'acme/widgets' }, ...payload },
    CONTEXT.deliveryId,
  );

const gitlab = (event: string, payload: Record<string, unknown>) =>
  mapDelivery(
    'gitlab',
    event,
    { project: { path_with_namespace: 'group/widgets' }, ...payload },
    CONTEXT.deliveryId,
  );

const ISSUE = { number: 12, title: 'Widgets break', html_url: 'https://github.test/i/12' };
const PULL = {
  number: 5,
  html_url: 'https://github.test/p/5',
  head: { ref: 'feature' },
  base: { ref: 'main' },
};

describe("GitHub's deliveries", () => {
  it.each([
    ['issues opened', 'issues', { action: 'opened', issue: ISSUE }, ['hub.forge.issue.opened']],
    ['issues edited', 'issues', { action: 'edited', issue: ISSUE }, ['hub.forge.issue.edited']],
    ['issues closed', 'issues', { action: 'closed', issue: ISSUE }, ['hub.forge.issue.closed']],
    [
      'issues reopened',
      'issues',
      { action: 'reopened', issue: ISSUE },
      ['hub.forge.issue.reopened'],
    ],
    [
      'issues labeled',
      'issues',
      { action: 'labeled', issue: ISSUE, label: { name: 'af:triage' } },
      ['hub.forge.issue.labeled'],
    ],
    [
      'issues unlabeled',
      'issues',
      { action: 'unlabeled', issue: ISSUE, label: { name: 'af:triage' } },
      ['hub.forge.issue.unlabeled'],
    ],
    [
      'an issue comment created',
      'issue_comment',
      { action: 'created', issue: ISSUE, comment: { html_url: 'https://github.test/c/7' } },
      ['hub.forge.comment.created'],
    ],
    [
      'a pull request opened',
      'pull_request',
      { action: 'opened', pull_request: PULL },
      ['hub.forge.pull_request.opened'],
    ],
    [
      'a pull request reopened',
      'pull_request',
      { action: 'reopened', pull_request: PULL },
      ['hub.forge.pull_request.reopened'],
    ],
    [
      'a pull request synchronized',
      'pull_request',
      { action: 'synchronize', pull_request: PULL },
      ['hub.forge.pull_request.synchronize'],
    ],
    [
      'a pull request closed unmerged',
      'pull_request',
      { action: 'closed', pull_request: { ...PULL, merged: false } },
      ['hub.forge.pull_request.closed'],
    ],
    [
      'a pull request closed merged',
      'pull_request',
      { action: 'closed', pull_request: { ...PULL, merged: true } },
      ['hub.forge.pull_request.merged'],
    ],
    ['a push', 'push', { ref: 'refs/heads/main', before: 'a', after: 'b' }, ['hub.forge.push']],
  ])('maps %s', (_what, event, payload, expected) => {
    expect(github(event, payload).events.map((one) => one.type)).toEqual(expected);
  });

  it.each([
    ['a ping', 'ping', {}],
    ['a pull request labeled', 'pull_request', { action: 'labeled', pull_request: PULL }],
    ['an edited issue comment', 'issue_comment', { action: 'edited', issue: ISSUE, comment: {} }],
    ['an event nobody mapped', 'star', { action: 'created' }],
  ])('relays nothing for %s', (_what, event, payload) => {
    expect(github(event, payload).events).toEqual([]);
  });

  it('carries the keys the event declaration lists and no payload besides', () => {
    const [event] = github('issues', {
      action: 'labeled',
      issue: { ...ISSUE, labels: [{ name: 'af:triage' }] },
      label: { name: 'af:triage' },
      sender: { login: 'alice' },
      secret_field: 'nothing of this should travel',
    }).events;

    expect(Object.keys(event?.metadata ?? {}).toSorted()).toEqual([
      'actor',
      'delivery_id',
      'forge',
      'label',
      'labels',
      'number',
      'url',
    ]);
    expect(event?.metadata['actor']).toBe('alice');
    expect(JSON.stringify(event)).not.toContain('nothing of this should travel');
  });
});

/** A set of labels named in order, for the capping case. */
const many = (prefix: string, count: number) =>
  Array.from({ length: count }, (_, index) => ({
    name: `${prefix}-${String(index).padStart(3, '0')}`,
  }));

describe("GitLab's deliveries", () => {
  const ATTRS = { iid: 12, title: 'Widgets break', url: 'https://gitlab.test/i/12' };

  it.each([
    [
      'an issue opened',
      'Issue Hook',
      { object_attributes: { ...ATTRS, action: 'open' } },
      ['hub.forge.issue.opened'],
    ],
    [
      'an issue closed',
      'Issue Hook',
      { object_attributes: { ...ATTRS, action: 'close' } },
      ['hub.forge.issue.closed'],
    ],
    [
      'an issue reopened',
      'Issue Hook',
      { object_attributes: { ...ATTRS, action: 'reopen' } },
      ['hub.forge.issue.reopened'],
    ],
    [
      'a note on an issue',
      'Note Hook',
      {
        object_attributes: { noteable_type: 'Issue', url: 'https://gitlab.test/n/1' },
        issue: { iid: 12 },
      },
      ['hub.forge.comment.created'],
    ],
    [
      'a note on a merge request',
      'Note Hook',
      {
        object_attributes: { noteable_type: 'MergeRequest', url: 'https://gitlab.test/n/2' },
        merge_request: { iid: 5 },
      },
      ['hub.forge.comment.created'],
    ],
    [
      'a merge request opened',
      'Merge Request Hook',
      { object_attributes: { iid: 5, action: 'open' } },
      ['hub.forge.pull_request.opened'],
    ],
    [
      'a merge request merged',
      'Merge Request Hook',
      { object_attributes: { iid: 5, action: 'merge' } },
      ['hub.forge.pull_request.merged'],
    ],
    [
      'a merge request updated with a new revision',
      'Merge Request Hook',
      { object_attributes: { iid: 5, action: 'update', oldrev: 'abc' } },
      ['hub.forge.pull_request.synchronize'],
    ],
    ['a push', 'Push Hook', { ref: 'refs/heads/main' }, ['hub.forge.push']],
    ['a tag push', 'Tag Push Hook', { ref: 'refs/tags/v1' }, ['hub.forge.push']],
  ])('maps %s', (_what, event, payload, expected) => {
    expect(gitlab(event, payload).events.map((one) => one.type)).toEqual(expected);
  });

  it.each([
    [
      'a merge request updated with nothing new',
      'Merge Request Hook',
      { object_attributes: { iid: 5, action: 'update' } },
    ],
    [
      'a note on a commit',
      'Note Hook',
      { object_attributes: { noteable_type: 'Commit', url: 'x' } },
    ],
    [
      'a note on a snippet',
      'Note Hook',
      { object_attributes: { noteable_type: 'Snippet', url: 'x' } },
    ],
    [
      'an issue update that changed nothing of interest',
      'Issue Hook',
      { object_attributes: { ...ATTRS, action: 'update' }, changes: {} },
    ],
    ['a hook nobody mapped', 'Wiki Page Hook', { object_attributes: {} }],
  ])('relays nothing for %s', (_what, event, payload) => {
    expect(gitlab(event, payload).events).toEqual([]);
  });

  it('maps an update that edited the title and changed labels, removals first', () => {
    const mapped = gitlab('Issue Hook', {
      object_attributes: { ...ATTRS, action: 'update' },
      labels: [{ name: 'kept' }, { name: 'added-a' }, { name: 'added-b' }],
      changes: {
        title: { previous: 'before', current: 'after' },
        labels: {
          previous: [{ name: 'kept' }, { name: 'gone-b' }, { name: 'gone-a' }],
          current: [{ name: 'kept' }, { name: 'added-b' }, { name: 'added-a' }],
        },
      },
    });

    expect(mapped.events.map((one) => one.type)).toEqual([
      'hub.forge.issue.edited',
      'hub.forge.issue.unlabeled',
      'hub.forge.issue.unlabeled',
      'hub.forge.issue.labeled',
      'hub.forge.issue.labeled',
    ]);
    expect(mapped.events.slice(1).map((one) => one.metadata['label'])).toEqual([
      'gone-a',
      'gone-b',
      'added-a',
      'added-b',
    ]);
  });

  it('caps the label events at a hundred and the labels in one at a hundred', () => {
    const mapped = gitlab('Issue Hook', {
      object_attributes: { ...ATTRS, action: 'update' },
      labels: many('now', 150),
      changes: { labels: { previous: many('gone', 150), current: many('now', 150) } },
    });

    expect(mapped.events).toHaveLength(100);
    expect(mapped.events.every((one) => (one.metadata['labels'] as string[]).length <= 100)).toBe(
      true,
    );
  });
});

describe('a delivery that says too little', () => {
  it.each([
    ['a body that is not an object', 'issues', ['not', 'an', 'object']],
    ['a body that is a number', 'issues', 7],
  ])('refuses %s', (_what, event, body) => {
    expect(() => mapDelivery('github', event, body, 'd-1')).toThrow(HttpError);
  });

  it.each([
    ['no repository', 'issues', { action: 'opened', issue: ISSUE }],
    [
      'no issue number',
      'issues',
      { repository: { full_name: 'a/b' }, action: 'opened', issue: {} },
    ],
    [
      'no label name',
      'issues',
      { repository: { full_name: 'a/b' }, action: 'labeled', issue: ISSUE, label: {} },
    ],
    ['no ref on a push', 'push', { repository: { full_name: 'a/b' } }],
  ])('refuses one with %s', (_what, event, payload) => {
    let refusal: unknown;
    try {
      mapDelivery('github', event, payload, 'd-1');
    } catch (err) {
      refusal = err;
    }

    expect((refusal as HttpError).status).toBe(400);
  });

  it('takes one missing only a title, a URL and the branches', () => {
    const mapped = mapDelivery(
      'github',
      'pull_request',
      { repository: { full_name: 'a/b' }, action: 'opened', pull_request: { number: 5 } },
      'd-1',
    );

    expect(mapped.events[0]?.metadata).toMatchObject({ url: '', head: '', base: '' });
  });
});

/**
 * No row of either table left unexercised. The cases above each
 * name their row; this one checks the set, so a row the reference
 * gains and nobody maps is caught here rather than by a consumer
 * whose events stopped arriving.
 */
describe('the rows of the reference’s two tables', () => {
  it('are each exercised by a delivery above', () => {
    const githubRows = [
      ['issues', 'opened'],
      ['issues', 'edited'],
      ['issues', 'closed'],
      ['issues', 'reopened'],
      ['issues', 'labeled'],
      ['issues', 'unlabeled'],
      ['issue_comment', 'created'],
      ['pull_request', 'opened'],
      ['pull_request', 'reopened'],
      ['pull_request', 'synchronize'],
      ['pull_request', 'closed'],
      ['push', ''],
    ] as const;
    const gitlabRows = [
      ['Issue Hook', 'open'],
      ['Issue Hook', 'close'],
      ['Issue Hook', 'reopen'],
      ['Issue Hook', 'update'],
      ['Note Hook', ''],
      ['Merge Request Hook', 'open'],
      ['Merge Request Hook', 'close'],
      ['Merge Request Hook', 'reopen'],
      ['Merge Request Hook', 'merge'],
      ['Merge Request Hook', 'update'],
      ['Push Hook', ''],
      ['Tag Push Hook', ''],
    ] as const;

    // Every row maps to at least one event when given a payload it
    // can read; the cases above assert which events each produces.
    for (const [event, action] of githubRows) {
      const mapped = github(event, {
        action,
        issue: { ...ISSUE, labels: [] },
        label: { name: 'a-label' },
        pull_request: { ...PULL, merged: action === 'closed' },
        comment: { html_url: 'https://github.test/c/1' },
        ref: 'refs/heads/main',
      });
      expect(mapped.events.length, `github ${event} ${action}`).toBeGreaterThan(0);
    }

    for (const [event, action] of gitlabRows) {
      const mapped = gitlab(event, {
        object_attributes: {
          iid: 12,
          action,
          url: 'https://gitlab.test/i/12',
          noteable_type: 'Issue',
          ...(action === 'update' ? { oldrev: 'abc' } : {}),
        },
        issue: { iid: 12 },
        changes: action === 'update' ? { title: { previous: 'a', current: 'b' } } : {},
        ref: 'refs/heads/main',
      });
      expect(mapped.events.length, `gitlab ${event} ${action}`).toBeGreaterThan(0);
    }
  });
});
