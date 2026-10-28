import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import {
  type Service,
  MOUNT,
  as,
  identityService,
  recordedEvents,
  withJson,
} from '../testutil/app.js';
import { type FakeForge, fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The nine writes, against a stub of each forge. Three of them make
 * more than one request, and for those what is proved is the order:
 * which request happened before the one that failed is what the
 * caller and the event have to agree about.
 */
let service: Service;
let forge: FakeForge;

const ISSUE = {
  number: 12,
  title: 'Widgets break',
  body: 'it breaks',
  state: 'open',
  user: { login: 'alice' },
  created_at: '2026-01-02T03:04:05Z',
  updated_at: '2026-01-02T03:04:05Z',
};

const GITLAB_ISSUE = {
  iid: 12,
  title: 'Widgets break',
  description: 'it breaks',
  state: 'opened',
  author: { username: 'alice' },
  created_at: '2026-01-02T03:04:05Z',
  updated_at: '2026-01-02T03:04:05Z',
};

const GITHUB_ANSWERS = {
  'POST /repos/acme/widgets/issues': { status: 201, body: ISSUE },
  'PATCH /repos/acme/widgets/issues/12': { status: 200, body: { ...ISSUE, state: 'closed' } },
  'POST /repos/acme/widgets/issues/12/comments': {
    status: 201,
    body: { id: 7, html_url: 'https://github.com/acme/widgets/issues/12#issuecomment-7' },
  },
  'POST /repos/acme/widgets/issues/12/labels': { status: 200, body: [{ name: 'af:triage' }] },
  'DELETE /repos/acme/widgets/issues/12/labels/af:triage': { status: 200, body: [] },
  'GET /repos/acme/widgets/labels/af:triage': { status: 404, body: { message: 'Not Found' } },
  'POST /repos/acme/widgets/labels': {
    status: 201,
    body: { name: 'af:triage', color: 'ededed', description: 'triage' },
  },
  'POST /repos/acme/widgets/pulls': {
    status: 201,
    body: {
      number: 5,
      title: 'Fix it',
      body: 'the fix',
      state: 'open',
      draft: false,
      merged: false,
      head: { sha: 'abc123', ref: 'feature' },
      base: { ref: 'main' },
      user: { login: 'alice' },
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
    },
  },
  'POST /repos/acme/widgets/pulls/5/comments': { status: 201, body: {} },
};

const GITLAB_ANSWERS = {
  'POST /api/v4/projects/group%2Fwidgets/issues': { status: 201, body: GITLAB_ISSUE },
  'PUT /api/v4/projects/group%2Fwidgets/issues/12': {
    status: 200,
    body: { ...GITLAB_ISSUE, state: 'closed' },
  },
  'POST /api/v4/projects/group%2Fwidgets/issues/12/notes': {
    status: 201,
    body: { id: 7 },
  },
  'GET /api/v4/projects/group%2Fwidgets/issues/12': { status: 200, body: GITLAB_ISSUE },
  'GET /api/v4/projects/group%2Fwidgets/labels/af:triage': {
    status: 404,
    body: { message: '404 Label Not Found' },
  },
  'POST /api/v4/projects/group%2Fwidgets/labels': {
    status: 201,
    body: { name: 'af:triage', color: '#ededed', description: 'triage' },
  },
  'POST /api/v4/projects/group%2Fwidgets/merge_requests': {
    status: 201,
    body: {
      iid: 5,
      title: 'Fix it',
      description: 'the fix',
      state: 'opened',
      sha: 'abc123',
      source_branch: 'feature',
      target_branch: 'main',
      author: { username: 'alice' },
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
    },
  },
  'POST /api/v4/projects/group%2Fwidgets/merge_requests/5/notes': { status: 201, body: { id: 9 } },
};

afterEach(() => {
  service.dispose();
});

async function world(which: 'github' | 'gitlab' = 'github', answers?: Record<string, unknown>) {
  forge = fakeForge({
    answers: (answers ?? (which === 'github' ? GITHUB_ANSWERS : GITLAB_ANSWERS)) as never,
  });
  service = await identityService([], { fetch: forge.fetch });
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url:
      which === 'github'
        ? 'https://github.com/acme/widgets.git'
        : 'https://gitlab.test/group/widgets.git',
  });
  await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'a-token');
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

type World = Awaited<ReturnType<typeof world>>;

const write = (held: World, path: string, body: unknown, method = 'POST') =>
  held.app.request(`${MOUNT}/workspaces/mine/${path}`, withJson(method, body, held.key));

describe.each(['github', 'gitlab'] as const)('writing to %s', (which) => {
  it.each([
    ['an issue', 'issues', { title: 'Widgets break', body: 'it breaks' }, 'POST', 201],
    ['an issue update', 'issues/12', { title: 'A new title' }, 'PATCH', 200],
    ['a close', 'issues/12/close', {}, 'POST', 200],
    ['a comment', 'issues/12/comments', { body: 'a comment' }, 'POST', 201],
    ['labels on an issue', 'issues/12/labels', ['af:triage'], 'POST', 204],
    ['a label of the repository', 'labels', { name: 'af:triage' }, 'POST', 201],
    ['a pull request', 'pulls', { title: 'Fix it', head: 'feature', base: 'main' }, 'POST', 201],
    ['a comment on a pull request', 'pulls/5/comments', { body: 'a comment' }, 'POST', 201],
  ])('answers %s with its documented status', async (_what, path, body, method, status) => {
    const held = await world(which);

    const response = await write(held, path, body, method);

    expect(response.status, await response.text()).toBe(status);
  });

  it('answers a label removal 204 and records one event for it', async () => {
    const held = await world(which);

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues/12/labels/af:triage`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    const events = await recordedEvents(held.store, 'hub.forge.label.remove');
    expect(events).toHaveLength(1);
    expect(events[0]?.metadata).toMatchObject({ label: 'af:triage', ok: true, target: 'origin' });
  });

  it('records exactly one event for each write that reached the forge', async () => {
    const held = await world(which);

    await write(held, 'issues', { title: 'Widgets break' });
    await write(held, 'issues/12/comments', { body: 'a comment' });

    const created = await recordedEvents(held.store, 'hub.forge.issue.create');
    const commented = await recordedEvents(held.store, 'hub.forge.comment.create');
    expect(created).toHaveLength(1);
    expect(commented).toHaveLength(1);
    expect(created[0]?.metadata).toMatchObject({ target: 'origin', ok: true, number: 12 });
  });
});

describe('a pull request’s head', () => {
  it('reaches the forge exactly as given, fork prefix and all', async () => {
    const held = await world('github');

    await write(held, 'pulls', { title: 'Fix it', head: 'someone:feature', base: 'main' });

    const sent = forge.last()?.body as { head?: string } | undefined;
    expect(sent?.head).toBe('someone:feature');
  });

  it('is created again when the caller asks again, there being no idempotency key', async () => {
    const held = await world('github');

    const first = await write(held, 'pulls', { title: 'Fix it', head: 'feature', base: 'main' });
    const second = await write(held, 'pulls', { title: 'Fix it', head: 'feature', base: 'main' });

    expect([first.status, second.status]).toEqual([201, 201]);
    expect(forge.calls().filter((call) => call.method === 'POST')).toHaveLength(2);
  });

  it('is answered 422 forge_rejected where the forge refuses a duplicate', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'POST /repos/acme/widgets/pulls': {
        status: 422,
        body: { message: 'A pull request already exists for acme:feature.' },
      },
    });

    const response = await write(held, 'pulls', { title: 'Fix it', head: 'feature', base: 'main' });
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(422);
    expect(body.error.error_type).toBe('forge_rejected');
    // A write that reached the forge is recorded even when it failed.
    const events = await recordedEvents(held.store, 'hub.forge.pull_request.create');
    expect(events[0]?.metadata).toMatchObject({ ok: false, error_type: 'forge_rejected' });
  });
});

describe('a close that carries a comment', () => {
  it('posts the comment first and then closes', async () => {
    const held = await world('github');

    const response = await write(held, 'issues/12/close', { comment: 'closing this' });

    expect(response.status).toBe(200);
    const posts = forge.calls().filter((call) => call.method !== 'GET');
    expect(posts[0]?.url).toContain('/issues/12/comments');
    expect(posts[1]?.url).toContain('/issues/12');
  });

  it('leaves the issue open when the comment is refused', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'POST /repos/acme/widgets/issues/12/comments': {
        status: 403,
        body: { message: 'no commenting here' },
      },
    });

    const response = await write(held, 'issues/12/close', { comment: 'closing this' });
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(502);
    expect(body.error.error_type).toBe('forge_forbidden');
    // Nothing was closed: the only write attempted was the comment.
    expect(forge.calls().filter((call) => call.method === 'PATCH')).toHaveLength(0);
  });

  it('keeps the comment and reports its URL when the close then fails', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'PATCH /repos/acme/widgets/issues/12': { status: 500, body: { message: 'boom' } },
    });

    const response = await write(held, 'issues/12/close', { comment: 'closing this' });

    expect(response.status).toBe(502);
    const [recorded] = await recordedEvents(held.store, 'hub.forge.issue.close');
    expect(recorded?.metadata).toMatchObject({
      ok: false,
      comment_url: 'https://github.com/acme/widgets/issues/12#issuecomment-7',
    });
  });

  it.each([
    ['no body at all', undefined],
    ['an empty body', {}],
    ['a blank comment', { comment: '   ' }],
  ])('closes without commenting for %s', async (_what, body) => {
    const held = await world('github');

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues/12/close`,
      body === undefined ? as(held.key, { method: 'POST' }) : withJson('POST', body, held.key),
    );

    expect(response.status).toBe(200);
    expect(forge.calls().some((call) => call.url.includes('/comments'))).toBe(false);
  });
});

describe('removing a label', () => {
  it('answers 204 for a label the issue never had', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'DELETE /repos/acme/widgets/issues/12/labels/af:triage': {
        status: 404,
        body: { message: 'Label does not exist' },
      },
      'GET /repos/acme/widgets/issues/12': { status: 200, body: ISSUE },
    });

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues/12/labels/af:triage`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    // The issue was read once to tell the two cases apart.
    expect(forge.calls().filter((call) => call.method === 'GET')).toHaveLength(1);
  });

  it('answers 404 forge_not_found where the issue itself is gone', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'DELETE /repos/acme/widgets/issues/12/labels/af:triage': {
        status: 404,
        body: { message: 'Not Found' },
      },
      'GET /repos/acme/widgets/issues/12': { status: 404, body: { message: 'Not Found' } },
    });

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues/12/labels/af:triage`,
      as(held.key, { method: 'DELETE' }),
    );
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(404);
    expect(body.error.error_type).toBe('forge_not_found');
  });

  it.each([
    ['a slash, encoded once', 'type%2Fbug', 'type/bug'],
    ['a colon, which needs none', 'af:fix', 'af:fix'],
  ])('takes the name from the last path segment with %s', async (_what, given, expected) => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      [`DELETE /repos/acme/widgets/issues/12/labels/${expected}`]: { status: 200, body: [] },
    });

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues/12/labels/${given}`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    const [recorded] = await recordedEvents(held.store, 'hub.forge.label.remove');
    expect(recorded?.metadata).toMatchObject({ label: expected });
  });
});

describe('ensuring a label exists', () => {
  it('answers 200 with the label the forge holds, unchanged', async () => {
    const held = await world('github', {
      ...GITHUB_ANSWERS,
      'GET /repos/acme/widgets/labels/af:triage': {
        status: 200,
        body: { name: 'af:triage', color: 'ff0000', description: 'as it was' },
      },
    });

    const response = await write(held, 'labels', {
      name: 'af:triage',
      color: '00ff00',
      description: 'something else',
    });
    const body = (await response.json()) as { color: string; description: string };

    expect(response.status).toBe(200);
    expect(body).toMatchObject({ color: 'ff0000', description: 'as it was' });
    expect(forge.calls().some((call) => call.method === 'POST')).toBe(false);
  });

  it('creates one that does not exist and answers 201', async () => {
    const held = await world('github');

    const response = await write(held, 'labels', { name: 'af:triage', description: 'triage' });

    expect(response.status).toBe(201);
    const [recorded] = await recordedEvents(held.store, 'hub.forge.label.create');
    expect(recorded?.metadata).toMatchObject({ label: 'af:triage', created: true });
  });

  it('reads once more and answers 200 where a concurrent call won the race', async () => {
    let created = false;
    const held = await world('github');
    forge.answerWith({
      ...GITHUB_ANSWERS,
      'GET /repos/acme/widgets/labels/af:triage': {
        get status() {
          // Absent the first time, there the second: the race.
          const answer = created ? 200 : 404;
          created = true;
          return answer;
        },
        body: { name: 'af:triage', color: 'ededed' },
      },
      'POST /repos/acme/widgets/labels': {
        status: 422,
        body: { message: 'already_exists' },
      },
    } as never);

    const response = await write(held, 'labels', { name: 'af:triage' });

    expect(response.status).toBe(200);
  });
});
