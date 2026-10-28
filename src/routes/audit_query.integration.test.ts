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
 * Hub events and agent events read as one timeline. The ordering is
 * the part no component test can see: two sources with clocks of
 * their own interleave correctly or they produce a timeline that
 * makes no sense, and nothing in between.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService([], { clock: 'ticking' });
  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  return { ...service, owner, org, key: (await seedApiKey(service.store, owner)).presented };
}

/** An agent event at an instant the suite chooses, written as given. */
async function agentEvent(
  held: Awaited<ReturnType<typeof world>>,
  id: string,
  timestamp: string,
): Promise<void> {
  const response = await held.app.request(
    `${MOUNT}/workspaces/mine/runs/${RUN}/events`,
    withJson('POST', { id, event_type: 'session.start', timestamp }, held.key),
  );
  expect(response.status).toBe(201);
}

/** A hub event at an instant the suite chooses. */
async function hubEvent(
  held: Awaited<ReturnType<typeof world>>,
  id: string,
  timestamp: string,
): Promise<void> {
  await held.store.run(
    `INSERT INTO hub_events (id, event_type, timestamp, severity, workspace, actor_id,
       actor_type, resource_type, resource_id, action, metadata)
     VALUES (?, 'hub.workspace.update', ?, 'info', 'mine', 'u-1', 'api_key', 'workspace',
       'mine', 'update', '{"fields":["display_name"]}')`,
    [id, new Date(timestamp).toISOString()],
  );
}

async function read(
  held: Awaited<ReturnType<typeof world>>,
  query = '',
  credential = held.key,
): Promise<{ ids: string[]; times: string[]; next: string | null }> {
  const response = await held.app.request(`${MOUNT}/audit${query}`, as(credential));
  expect(response.status, `${query} answered ${String(response.status)}`).toBe(200);
  const body = (await response.json()) as {
    events: Array<{ id: string; timestamp: string }>;
    next_cursor: string | null;
  };
  return {
    ids: body.events.map((one) => one.id),
    times: body.events.map((one) => one.timestamp),
    next: body.next_cursor,
  };
}

describe('two sources read as one timeline', () => {
  it('interleaves them by instant, through the same second and differing precision', async () => {
    const held = await world();
    await hubEvent(held, 'h-1', '2026-07-04T14:00:00Z');
    await agentEvent(held, 'a-1', '2026-07-04T14:00:00.250Z');
    await hubEvent(held, 'h-2', '2026-07-04T14:00:00.500Z');
    await agentEvent(held, 'a-2', '2026-07-04T14:00:00.750Z');
    await hubEvent(held, 'h-3', '2026-07-04T14:00:01Z');

    const { ids, times } = await read(held);

    expect(ids).toEqual(['h-3', 'a-2', 'h-2', 'a-1', 'h-1']);
    expect(times).toEqual([...times].toSorted().toReversed());
  });

  it('pages to the end across both sources, with every event once', async () => {
    const held = await world();
    for (let index = 0; index < 6; index += 1) {
      const at = `2026-07-04T14:00:0${String(index)}.000Z`;
      await (index % 2 === 0 ? hubEvent : agentEvent)(held, `e-${String(index)}`, at);
    }

    const seen: string[] = [];
    let query = '?limit=2';
    for (let page = 0; page < 10; page += 1) {
      const { ids, next } = await read(held, query);
      seen.push(...ids);
      if (next === null) {
        break;
      }
      query = `?limit=2&cursor=${next}`;
    }

    expect(seen).toHaveLength(6);
    expect(new Set(seen).size).toBe(6);
    expect(seen).toEqual(['e-5', 'e-4', 'e-3', 'e-2', 'e-1', 'e-0']);
  });
});

describe('whose events a caller is shown', () => {
  it('shows every workspace they own or share, archived ones included', async () => {
    const held = await world();
    await hubEvent(held, 'h-mine', '2026-07-04T14:00:00Z');

    // One they own that is archived, one they share through the org,
    // one of a stranger's, and one belonging to no workspace at all.
    await seedWorkspace(held.store, {
      slug: 'retired',
      owner_id: held.owner.id,
      status: 'archived',
    });
    const other = await seedUser(held.store);
    await addMember(held.store, {
      org_id: held.org.id,
      user_id: other.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    await seedWorkspace(held.store, {
      slug: 'shared',
      owner_id: other.id,
      org_id: held.org.id,
      status: 'ready',
    });
    const stranger = await seedUser(held.store);
    await seedWorkspace(held.store, { slug: 'theirs', owner_id: stranger.id, status: 'ready' });

    for (const [id, slug] of [
      ['h-retired', 'retired'],
      ['h-shared', 'shared'],
      ['h-theirs', 'theirs'],
    ] as const) {
      await held.store.run(
        `INSERT INTO hub_events (id, event_type, timestamp, severity, workspace, actor_id,
           actor_type, resource_type, resource_id, action, metadata)
         VALUES (?, 'hub.workspace.update', '2026-07-04T14:00:00.000Z', 'info', ?, 'u-1',
           'api_key', 'workspace', ?, 'update', '{}')`,
        [id, slug, slug],
      );
    }
    await held.store.run(
      `INSERT INTO hub_events (id, event_type, timestamp, severity, workspace, actor_id,
         actor_type, resource_type, resource_id, action, metadata)
       VALUES ('h-none', 'hub.token.bind', '2026-07-04T14:00:00.000Z', 'info', NULL, 'u-1',
         'api_key', 'token', 't-1', 'bind', '{}')`,
    );

    const mine = await read(held);
    const admin = await read(held, '', ADMIN_TOKEN);

    expect(mine.ids.toSorted()).toEqual(['h-mine', 'h-retired', 'h-shared']);
    expect(admin.ids.toSorted()).toEqual(['h-mine', 'h-none', 'h-retired', 'h-shared', 'h-theirs']);
  });
});
