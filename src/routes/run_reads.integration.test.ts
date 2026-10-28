import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Reading back a run that really holds records: the filters each
 * route offers, the order it answers in, and paging that neither
 * repeats a record nor loses one.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService([], { clock: 'ticking' });
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  const key = (await seedApiKey(service.store, owner)).presented;

  // Ten events across two nodes, two sessions and two types.
  for (let index = 0; index < 10; index += 1) {
    await service.app.request(
      path('events'),
      withJson(
        'POST',
        {
          id: `e-${String(index)}`,
          event_type: index % 2 === 0 ? 'session.start' : 'session.fail',
          node_id: `node-${String(index % 2)}`,
          session_id: `s-${String(index % 2)}`,
          timestamp: `2026-07-04T14:${String(index).padStart(2, '0')}:00Z`,
        },
        key,
      ),
    );
  }
  return { ...service, key };
}

const path = (route: string, query = '') => `${MOUNT}/workspaces/mine/runs/${RUN}/${route}${query}`;

async function listed(
  held: Awaited<ReturnType<typeof world>>,
  query: string,
): Promise<{ ids: string[]; next: string | null; more: boolean }> {
  const body = (await (await held.app.request(path('events', query), as(held.key))).json()) as {
    events: Array<{ id: string }>;
    next_cursor: string | null;
    has_more: boolean;
  };
  return { ids: body.events.map((one) => one.id), next: body.next_cursor, more: body.has_more };
}

describe('a run’s events, listed', () => {
  it('answers newest first by default', async () => {
    const held = await world();

    const { ids } = await listed(held, '');

    expect(ids).toEqual(['e-9', 'e-8', 'e-7', 'e-6', 'e-5', 'e-4', 'e-3', 'e-2', 'e-1', 'e-0']);
  });

  it.each([
    ['?event_type=session.fail', ['e-9', 'e-7', 'e-5', 'e-3', 'e-1']],
    ['?severity=error', ['e-9', 'e-7', 'e-5', 'e-3', 'e-1']],
    ['?node_id=node-0', ['e-8', 'e-6', 'e-4', 'e-2', 'e-0']],
    ['?session_id=s-1', ['e-9', 'e-7', 'e-5', 'e-3', 'e-1']],
    ['?since=2026-07-04T14:08:00Z', ['e-9', 'e-8']],
    ['?until=2026-07-04T14:02:00Z', ['e-1', 'e-0']],
    ['?order=asc&limit=2', ['e-0', 'e-1']],
  ])('narrows to what %s asks for', async (query, expected) => {
    const held = await world();

    expect((await listed(held, query)).ids).toEqual(expected);
  });

  it('pages to the end, with every record once and none twice', async () => {
    const held = await world();

    const seen: string[] = [];
    let query = '?limit=3';
    for (let page = 0; page < 10; page += 1) {
      const { ids, next, more } = await listed(held, query);
      seen.push(...ids);
      if (next === null) {
        expect(more).toBe(false);
        break;
      }
      expect(more).toBe(true);
      query = `?limit=3&cursor=${next}`;
    }

    expect(seen).toHaveLength(10);
    expect(new Set(seen).size).toBe(10);
  });

  it('answers at most five hundred, and fifty when nothing is asked', async () => {
    const held = await world();

    const fifty = await held.app.request(path('events'), as(held.key));
    const most = await held.app.request(path('events', '?limit=500'), as(held.key));

    expect(fifty.status).toBe(200);
    expect(most.status).toBe(200);
    expect((await held.app.request(path('events', '?limit=501'), as(held.key))).status).toBe(400);
  });
});

describe('the other four lists', () => {
  it('each answer with their own collection and their own filters', async () => {
    const held = await world();
    for (const [route, body] of [
      ['sessions/outcomes', { session_id: 's-1', status: 'completed', node_id: 'n-1' }],
      ['tools/calls', { tool_name: 'bash', session_id: 's-1' }],
      ['tools/errors', { tool_name: 'bash', error_msg: 'no', session_id: 's-1' }],
      ['traces', { event_type: 'session.init', session_id: 's-1' }],
    ] as const) {
      await held.app.request(path(route), withJson('POST', body, held.key));
    }

    const read = async (route: string, named: string, query: string) =>
      (
        (await (await held.app.request(path(route, query), as(held.key))).json()) as Record<
          string,
          unknown[]
        >
      )[named];

    expect(await read('sessions/outcomes', 'outcomes', '?status=completed')).toHaveLength(1);
    expect(await read('sessions/outcomes', 'outcomes', '?status=failed')).toHaveLength(0);
    expect(await read('tools/calls', 'calls', '?tool_name=bash')).toHaveLength(1);
    expect(await read('tools/calls', 'calls', '?tool_name=python')).toHaveLength(0);
    expect(await read('tools/errors', 'errors', '?session_id=s-1')).toHaveLength(1);
    expect(await read('traces', 'traces', '?event_type=session.init')).toHaveLength(1);
    expect(await read('traces', 'traces', '?node_id=elsewhere')).toHaveLength(0);
  });
});
