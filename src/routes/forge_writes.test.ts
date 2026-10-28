import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { type FakeForge, fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What an issue update sends on. Only the fields the caller named may
 * change: a field they left out is one the forge must not hear about,
 * because hearing about it with a null would clear it.
 */
let service: Service;
let forge: FakeForge;

afterEach(() => {
  service.dispose();
});

const ISSUE = {
  number: 12,
  title: 'Widgets break',
  body: 'it breaks',
  state: 'open',
  user: { login: 'alice' },
  created_at: '2026-01-02T03:04:05Z',
  updated_at: '2026-01-02T03:04:05Z',
};

async function world(which: 'github' | 'gitlab' = 'github') {
  forge = fakeForge({ fallback: { status: 200, body: ISSUE } });
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

const update = (held: Awaited<ReturnType<typeof world>>, body: unknown) =>
  held.app.request(`${MOUNT}/workspaces/mine/issues/12`, withJson('PATCH', body, held.key));

describe('an issue update', () => {
  it.each([
    ['a title alone', { title: 'A new title' }, { title: 'A new title' }],
    ['a body cleared', { body: '' }, { body: '' }],
    ['a state', { state: 'closed' }, { state: 'closed' }],
    ['a whole new label set', { labels: ['one', 'two'] }, { labels: ['one', 'two'] }],
    ['every label removed', { labels: [] }, { labels: [] }],
    ['every assignee removed', { assignees: [] }, { assignees: [] }],
    [
      'a title with a null body, which is left alone',
      { title: 'A new title', body: null },
      { title: 'A new title' },
    ],
  ])('sends %s and nothing else', async (_what, body, expected) => {
    const held = await world();

    const response = await update(held, body);

    expect(response.status).toBe(200);
    expect(forge.last()?.body).toEqual(expected);
  });

  it.each([
    ['a body naming nothing it can change', {}],
    ['a body naming only fields it does not know', { milestone: 3 }],
    ['every field null', { title: null, body: null, state: null }],
  ])('answers 400 nothing to update for %s', async (_what, body) => {
    const held = await world();
    const before = forge.calls().length;

    const response = await update(held, body);
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message).toContain('nothing to update');
    expect(forge.calls().length).toBe(before);
  });

  it('refuses a blank title and a state it does not know', async () => {
    const held = await world();

    expect((await update(held, { title: '   ' })).status).toBe(400);
    expect((await update(held, { state: 'sideways' })).status).toBe(400);
  });

  it('refuses an update that sets only assignees on GitLab, which cannot take them', async () => {
    const held = await world('gitlab');

    const response = await update(held, { assignees: ['alice'] });
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message).toContain('nothing to update');
  });

  it('accepts assignees beside another field on GitLab, recording only the other', async () => {
    const held = await world('gitlab');

    const response = await update(held, { title: 'A new title', assignees: ['alice'] });

    expect(response.status).toBe(200);
  });
});

describe('a write body that breaks its route’s rules', () => {
  it.each([
    ['an issue with no title', 'issues', 'POST', { body: 'no title' }],
    ['an issue with a blank title', 'issues', 'POST', { title: '  ' }],
    [
      'an issue with more than a hundred labels',
      'issues',
      'POST',
      {
        title: 'ok',
        labels: Array.from({ length: 101 }, (_, index) => `l-${String(index)}`),
      },
    ],
    ['an issue with a blank label', 'issues', 'POST', { title: 'ok', labels: ['ok', ' '] }],
    ['a comment with no body', 'issues/12/comments', 'POST', {}],
    ['a comment that is blank', 'issues/12/comments', 'POST', { body: '   ' }],
    ['labels given as an object', 'issues/12/labels', 'POST', { labels: ['one'] }],
    ['an empty label array', 'issues/12/labels', 'POST', []],
    ['a label array holding a number', 'issues/12/labels', 'POST', ['one', 2]],
    ['a label with no name', 'labels', 'POST', { color: 'ededed' }],
    ['a label colour that is not six hex digits', 'labels', 'POST', { name: 'x', color: 'red' }],
    ['a pull request with no head', 'pulls', 'POST', { title: 'Fix it', base: 'main' }],
    ['a pull request with a blank base', 'pulls', 'POST', { title: 'Fix', head: 'f', base: ' ' }],
    [
      'a pull request whose draft is no boolean',
      'pulls',
      'POST',
      {
        title: 'Fix',
        head: 'f',
        base: 'main',
        draft: 'yes',
      },
    ],
  ])('answers 400 to %s, reaching no forge', async (_what, path, method, body) => {
    const held = await world();
    const before = forge.calls().length;

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/${path}`,
      withJson(method, body, held.key),
    );

    expect(response.status).toBe(400);
    expect(forge.calls().length).toBe(before);
  });
});
