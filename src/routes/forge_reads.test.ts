import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { type FakeForge, fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What the issue list sends on, and what it refuses. A parameter the
 * caller did not give must not be sent: the forge's own default is
 * the answer then, and sending one of ours would change the result
 * without anybody asking.
 */
let service: Service;
let forge: FakeForge;

afterEach(() => {
  service.dispose();
});

async function world() {
  forge = fakeForge({ fallback: { body: [] } });
  service = await identityService([], { fetch: forge.fetch });
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://github.com/acme/widgets.git',
  });
  await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'a-token');
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

/** The query the forge was sent, as a plain object. */
function sentQuery(): Record<string, string> {
  const last = forge.last();
  const sent: Record<string, string> = {};
  for (const [name, value] of new URL(last?.url ?? 'https://x.test').searchParams) {
    sent[name] = value;
  }
  return sent;
}

describe('the issue-list parameters that reach the forge', () => {
  it.each([
    ['nothing at all', '', { per_page: '30' }],
    ['a state', '?state=closed', { state: 'closed', per_page: '30' }],
    ['an assignee', '?assignee=alice', { assignee: 'alice', per_page: '30' }],
    ['a sort', '?sort=updated', { sort: 'updated', per_page: '30' }],
    ['a direction', '?direction=asc', { direction: 'asc', per_page: '30' }],
    ['a limit', '?limit=7', { per_page: '7' }],
    ['labels', '?labels=one,two', { labels: 'one,two', per_page: '30' }],
    ['labels with empty items', '?labels=one,,two,', { labels: 'one,two', per_page: '30' }],
    ['labels that are all empty', '?labels=,,', { per_page: '30' }],
    [
      'every parameter together',
      '?state=all&labels=one&assignee=alice&sort=created&direction=desc&limit=100',
      {
        state: 'all',
        labels: 'one',
        assignee: 'alice',
        sort: 'created',
        direction: 'desc',
        per_page: '100',
      },
    ],
  ])('sends %s', async (_what, query, expected) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/issues${query}`,
      as(held.key),
    );

    expect(response.status).toBe(200);
    expect(sentQuery()).toEqual(expected);
  });
});

describe('a read the service refuses before the forge', () => {
  it.each([
    ['a path number of zero', 'issues/0', 'positive decimal integer'],
    ['a negative path number', 'issues/-1', 'positive decimal integer'],
    ['a path number that is a word', 'issues/abc', 'positive decimal integer'],
    ['a path number with a fraction', 'issues/1.5', 'positive decimal integer'],
    ['a pull number that is a word', 'pulls/abc/files', 'positive decimal integer'],
    ['a state it does not know', 'issues?state=sideways', 'state'],
    ['an empty state', 'issues?state=', 'state'],
    ['a repeated state', 'issues?state=open&state=closed', 'state'],
    ['a limit of zero', 'issues?limit=0', 'limit'],
    ['a limit past a hundred', 'issues?limit=101', 'limit'],
    ['a limit that is no number', 'issues?limit=many', 'limit'],
    ['a sort it does not know', 'issues?sort=votes', 'sort'],
    ['a direction it does not know', 'issues?direction=up', 'direction'],
    ['an empty assignee', 'issues?assignee=', 'assignee'],
  ])('answers 400 to %s, reaching no forge', async (_what, path, named) => {
    const held = await world();
    const before = forge.calls().length;

    const response = await held.app.request(`${MOUNT}/workspaces/mine/${path}`, as(held.key));
    const body = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(body.error.message).toContain(named);
    expect(forge.calls().length).toBe(before);
  });
});
