import { afterEach, describe, expect, it } from 'vitest';
import { ADMIN_TOKEN, type Service, identityService } from '../testutil/app.js';
import { addMember } from '../store/identity.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What a workspace's agents have cost. The arithmetic matters less
 * than the agreement: the totals and the breakdown come from the same
 * rows, and a reader should never have to reconcile them.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  slug: string;
  archived: string;
  owner: string;
  member: string;
  stranger: string;
}

async function world(): Promise<World> {
  const service = await identityService([], { clock: 'ticking' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(service.store);

  return {
    service,
    slug: (
      await seedWorkspace(service.store, { owner_id: owner.id, org_id: org.id, status: 'ready' })
    ).slug,
    archived: (
      await seedWorkspace(service.store, {
        owner_id: owner.id,
        org_id: org.id,
        status: 'archived',
      })
    ).slug,
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    stranger: (await seedApiKey(service.store, stranger)).presented,
  };
}

/**
 * A session with usage recorded at a stated time, written through
 * the store so the suite can place it on a chosen day.
 */
async function spent(
  place: World,
  slug: string,
  model: string,
  tokens: { input: number; output: number; cache?: number },
  on: string,
): Promise<string> {
  const id = `sess-${String(Math.random()).slice(2, 10)}`;
  await place.service.store.run(
    `INSERT INTO sessions (id, workspace_slug, credential_type, status, started_at)
     VALUES (?, ?, 'api_key', 'completed', ?)`,
    [id, slug, on],
  );
  await place.service.store.run(
    `INSERT INTO session_usage
       (id, session_id, model, input_tokens, output_tokens, cache_read_tokens, recorded_at)
     VALUES (?, ?, ?, ?, ?, ?, ?)`,
    [`u-${id}`, id, model, tokens.input, tokens.output, tokens.cache ?? 0, on],
  );
  return id;
}

async function cost(place: World, query = '', credential?: string): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/cost${query}`, {
    headers: { authorization: `Bearer ${credential ?? place.owner}` },
  });
}

const WIDE = '?since=2026-01-01T00:00:00Z&until=2027-01-01T00:00:00Z';

describe("a workspace's cost", () => {
  it('groups by day, and the totals agree with the breakdown', async () => {
    const place = await world();
    await spent(place, place.slug, 'a-model', { input: 100, output: 40 }, '2026-06-01T10:00:00Z');
    await spent(place, place.slug, 'a-model', { input: 200, output: 60 }, '2026-06-01T14:00:00Z');
    await spent(place, place.slug, 'another', { input: 50, output: 10 }, '2026-06-02T09:00:00Z');

    const body = (await (await cost(place, WIDE)).json()) as {
      workspace: string;
      totals: Record<string, number>;
      breakdown: Array<Record<string, unknown>>;
    };

    expect(body.workspace).toBe(place.slug);
    expect(body.breakdown.map((one) => one['date'])).toEqual(['2026-06-01', '2026-06-02']);
    expect(body.totals['input_tokens']).toBe(350);
    expect(body.totals['output_tokens']).toBe(110);
    expect(body.totals['sessions']).toBe(3);
    expect(body.breakdown.reduce((sum, one) => sum + Number(one['input_tokens']), 0)).toBe(
      body.totals['input_tokens'],
    );
  });

  it.each([
    ['session', 'session_id', 3],
    ['model', 'model', 2],
  ])('groups by %s', async (grouping, field, rows) => {
    const place = await world();
    await spent(place, place.slug, 'a-model', { input: 100, output: 40 }, '2026-06-01T10:00:00Z');
    await spent(place, place.slug, 'a-model', { input: 200, output: 60 }, '2026-06-01T14:00:00Z');
    await spent(place, place.slug, 'another', { input: 50, output: 10 }, '2026-06-02T09:00:00Z');

    const body = (await (await cost(place, `${WIDE}&group_by=${grouping}`)).json()) as {
      totals: Record<string, number>;
      breakdown: Array<Record<string, unknown>>;
    };

    expect(body.breakdown).toHaveLength(rows);
    expect(body.breakdown.every((one) => one[field] !== undefined)).toBe(true);
    expect(body.totals['input_tokens']).toBe(350);
  });

  it('counts one session spanning two days as one session', async () => {
    const place = await world();
    const id = await spent(
      place,
      place.slug,
      'a-model',
      { input: 100, output: 10 },
      '2026-06-01T23:00:00Z',
    );
    await place.service.store.run(
      `INSERT INTO session_usage
         (id, session_id, model, input_tokens, output_tokens, cache_read_tokens, recorded_at)
       VALUES ('second', ?, 'a-model', 50, 5, 0, '2026-06-02T01:00:00Z')`,
      [id],
    );

    const body = (await (await cost(place, WIDE)).json()) as {
      totals: Record<string, number>;
      breakdown: Array<Record<string, unknown>>;
    };

    expect(body.breakdown).toHaveLength(2);
    expect(body.totals['sessions']).toBe(1);
  });

  it('keeps to the period it was asked about', async () => {
    const place = await world();
    await spent(place, place.slug, 'a-model', { input: 100, output: 10 }, '2026-06-01T10:00:00Z');
    await spent(place, place.slug, 'a-model', { input: 999, output: 99 }, '2026-07-01T10:00:00Z');

    const body = (await (
      await cost(place, '?since=2026-06-01T00:00:00Z&until=2026-06-30T00:00:00Z')
    ).json()) as { totals: Record<string, number> };

    expect(body.totals['input_tokens']).toBe(100);
  });

  it('reports nothing spent as nothing, not as an error', async () => {
    const place = await world();

    const response = await cost(place, WIDE);
    const body = (await response.json()) as {
      totals: Record<string, number>;
      breakdown: unknown[];
    };

    expect(response.status).toBe(200);
    expect(body.breakdown).toEqual([]);
    expect(body.totals['input_tokens']).toBe(0);
    expect(body.totals['sessions']).toBe(0);
  });
});

describe('a cost report the route will not make', () => {
  it.each([
    ['a grouping nobody has', '?group_by=phase-of-the-moon'],
    [
      'a period that ends before it starts',
      '?since=2026-07-01T00:00:00Z&until=2026-06-01T00:00:00Z',
    ],
    ['a period of no length', '?since=2026-06-01T00:00:00Z&until=2026-06-01T00:00:00Z'],
    ['a since that is not a time', '?since=whenever'],
  ])('refuses %s', async (_what, query) => {
    const place = await world();

    expect((await cost(place, query)).status).toBe(400);
  });

  it('refuses a caller who may not read the workspace, and an archived one', async () => {
    const place = await world();

    const hidden = await cost(place, WIDE, place.stranger);
    const inactive = await place.service.app.request(
      `/api/v1/workspaces/${place.archived}/cost${WIDE}`,
      { headers: { authorization: `Bearer ${place.owner}` } },
    );

    for (const response of [hidden, inactive]) {
      expect(response.status).toBe(403);
      expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
        'workspace access denied',
      );
    }
  });

  it('serves a member of an active workspace, and an admin token anywhere', async () => {
    const place = await world();

    expect((await cost(place, WIDE, place.member)).status).toBe(200);
    expect(
      (
        await place.service.app.request(`/api/v1/workspaces/${place.archived}/cost${WIDE}`, {
          headers: { authorization: `Bearer ${ADMIN_TOKEN}` },
        })
      ).status,
    ).toBe(200);
  });
});
