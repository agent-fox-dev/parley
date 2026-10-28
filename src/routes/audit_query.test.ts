import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The one query over both sources. Its filters are the part that is
 * easy to get subtly wrong: a filter only hub events carry leaves the
 * agent events out rather than matching none of them, and that is
 * deliberate.
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

  // One hub event, by doing something that records one.
  await service.app.request(
    `${MOUNT}/workspaces/mine/vars`,
    withJson('POST', { entries: [{ key: 'A_VAR', value: 'v' }] }, key),
  );
  // And one of the agent's own.
  await service.app.request(
    `${MOUNT}/workspaces/mine/runs/${RUN}/events`,
    withJson('POST', { event_type: 'session.start', node_id: 'n-1' }, key),
  );

  return { ...service, key };
}

async function sources(held: Awaited<ReturnType<typeof world>>, query: string): Promise<string[]> {
  const response = await held.app.request(`${MOUNT}/audit${query}`, as(held.key));
  expect(response.status, `${query} answered ${String(response.status)}`).toBe(200);
  const body = (await response.json()) as { events: Array<{ source: string }> };
  return body.events.map((one) => one.source).toSorted();
}

describe('what a filter leaves out', () => {
  it('carries both sources when nothing narrows it', async () => {
    const held = await world();

    expect(await sources(held, '')).toEqual(['agent', 'hub']);
  });

  it.each([
    ['?source=hub', ['hub']],
    ['?source=agent', ['agent']],
    ['?actor_type=api_key', ['hub']],
    ['?resource_type=variable', ['hub']],
    ['?action=create', ['hub']],
    [`?run_id=${RUN}`, ['agent']],
    ['?event_type=hub.variable.create', ['hub']],
    ['?event_type_prefix=hub.variable', ['hub']],
    ['?event_type_prefix=session', ['agent']],
  ])('%s narrows to %s', async (query, expected) => {
    const held = await world();

    expect(await sources(held, query)).toEqual(expected);
  });

  it('leaves agent events out for an actor nobody has, rather than matching none', async () => {
    const held = await world();

    expect(await sources(held, '?actor_id=nobody')).toEqual([]);
  });
});

describe('a query the service refuses', () => {
  it.each([
    ['a cursor it did not issue', '?cursor=not-ours', 400],
    ['a malformed since', '?since=yesterday', 400],
    ['a since equal to the until', '?since=2026-07-04T00:00:00Z&until=2026-07-04T00:00:00Z', 400],
    ['a since after the until', '?since=2026-07-05T00:00:00Z&until=2026-07-04T00:00:00Z', 400],
    ['a limit past the maximum', '?limit=1001', 400],
    ['a workspace the caller cannot read', '?workspace=elsewhere', 404],
  ])('answers %s with %i', async (_what, query, status) => {
    const held = await world();

    const response = await held.app.request(`${MOUNT}/audit${query}`, as(held.key));

    expect(response.status).toBe(status);
  });

  it('answers a workspace that exists but is out of reach the same as one that does not', async () => {
    const held = await world();
    const other = await seedUser(held.store);
    await seedWorkspace(held.store, { slug: 'theirs', owner_id: other.id, status: 'ready' });

    const unknown = await held.app.request(`${MOUNT}/audit?workspace=nowhere`, as(held.key));
    const foreign = await held.app.request(`${MOUNT}/audit?workspace=theirs`, as(held.key));

    expect([unknown.status, foreign.status]).toEqual([404, 404]);
  });
});

describe('an event of no workspace', () => {
  it('reaches an admin token and nobody else', async () => {
    const held = await world();
    await held.store.run(
      `INSERT INTO hub_events (id, event_type, timestamp, severity, workspace, actor_id,
         actor_type, resource_type, resource_id, action, metadata)
       VALUES ('e-nowhere', 'hub.token.bind', '2026-07-04T14:00:00.000Z', 'info', NULL,
         'u-1', 'api_key', 'token', 't-1', 'bind', '{}')`,
    );

    const mine = (await (await held.app.request(`${MOUNT}/audit`, as(held.key))).json()) as {
      events: Array<{ id: string; workspace: string | null }>;
    };
    const admin = (await (await held.app.request(`${MOUNT}/audit`, as(ADMIN_TOKEN))).json()) as {
      events: Array<{ id: string }>;
    };

    expect(mine.events.every((one) => one.workspace !== null)).toBe(true);
    expect(mine.events.map((one) => one.id)).not.toContain('e-nowhere');
    expect(admin.events.map((one) => one.id)).toContain('e-nowhere');
  });
});
