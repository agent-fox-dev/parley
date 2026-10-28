import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';

/**
 * Who may read a run's records, and what a read that asks for
 * nonsense is told. The reads are open where the writes are not: a
 * member of the organization reads them, and an archived workspace is
 * read like any other.
 */
const RUN = '20260704_143022_a1b2c3';

/** The five list routes and the collection each answers with. */
const LISTS = [
  ['events', 'events'],
  ['sessions/outcomes', 'outcomes'],
  ['tools/calls', 'calls'],
  ['tools/errors', 'errors'],
  ['traces', 'traces'],
] as const;

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  const member = await seedUser(store);
  await addMember(store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(store);
  for (const [slug, status] of [
    ['active', 'ready'],
    ['retired', 'archived'],
  ] as const) {
    await seedWorkspace(store, { slug, owner_id: owner.id, org_id: org.id, status });
  }

  return {
    ...service,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    strangerKey: (await seedApiKey(store, stranger)).presented,
  };
}

const path = (slug: string, route: string, query = '') =>
  `${MOUNT}/workspaces/${slug}/runs/${RUN}/${route}${query}`;

describe('who may read a run’s records', () => {
  it.each([
    ['the owner', 'ownerKey', 'active', 200],
    ['the owner of an archived workspace', 'ownerKey', 'retired', 200],
    ['a member', 'memberKey', 'active', 200],
    ['a member of an archived workspace', 'memberKey', 'retired', 200],
    ['a stranger', 'strangerKey', 'active', 404],
    ['anyone, for a workspace that is not there', 'ownerKey', 'nowhere', 404],
  ])('answers %s with %i on every list', async (_who, credential, slug, status) => {
    const held = await world();

    for (const [route] of LISTS) {
      const response = await held.app.request(
        path(slug, route),
        as(held[credential as 'ownerKey']),
      );
      expect(response.status, `${route} answered ${String(response.status)}`).toBe(status);
    }
  });

  it('serves an admin token, whatever the workspace’s status', async () => {
    const held = await world();

    for (const slug of ['active', 'retired']) {
      expect((await held.app.request(path(slug, 'events'), as(ADMIN_TOKEN))).status).toBe(200);
    }
  });
});

describe('a read that asks for nonsense', () => {
  it.each([
    ['a run id of the wrong shape', `${MOUNT}/workspaces/active/runs/nonsense/events`],
    ['a cursor the service did not issue', path('active', 'events', '?cursor=not-ours')],
    ['a malformed since', path('active', 'events', '?since=yesterday')],
    [
      'an until before the since',
      path('active', 'events', '?since=2026-07-04T00:00:00Z&until=2026-07-03T00:00:00Z'),
    ],
    ['a limit past the maximum', path('active', 'events', '?limit=501')],
    ['a limit that is not a number', path('active', 'events', '?limit=plenty')],
    ['an order it does not know', path('active', 'events', '?order=sideways')],
  ])('answers 400 to %s', async (_what, url) => {
    const held = await world();

    const response = await held.app.request(url, as(held.ownerKey));

    expect(response.status).toBe(400);
  });
});

describe('an empty run', () => {
  it.each(LISTS)('answers %s with an empty page rather than a refusal', async (route, named) => {
    const held = await world();

    const response = await held.app.request(path('active', route), as(held.ownerKey));
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(body[named]).toEqual([]);
    expect(body['next_cursor']).toBeNull();
    expect(body['has_more']).toBe(false);
  });
});

describe('what a list carries', () => {
  it('unpacks the JSON an agent sent rather than answering with its text', async () => {
    const held = await world();
    await held.app.request(
      path('active', 'tools/calls'),
      withJson(
        'POST',
        { tool_name: 'bash', input: { cmd: 'ls' }, output: { code: 0 } },
        held.ownerKey,
      ),
    );

    const body = (await (
      await held.app.request(path('active', 'tools/calls'), as(held.ownerKey))
    ).json()) as { calls: Array<Record<string, unknown>> };

    expect(body.calls[0]?.['input']).toEqual({ cmd: 'ls' });
    expect(body.calls[0]?.['output']).toEqual({ code: 0 });
    expect(body.calls[0]?.['tool_name']).toBe('bash');
  });
});
