import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { recordDelivery } from '../webhook/log.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What the delivery log takes as parameters. A log is read when
 * something has gone wrong, so a parameter it cannot read is said
 * plainly rather than ignored.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world(count = 40) {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });

  for (let index = 0; index < count; index += 1) {
    await recordDelivery(service.store, {
      workspace_slug: 'mine',
      delivery_id: `d-${String(index).padStart(3, '0')}`,
      forge: 'github',
      event: 'issues',
      action: 'opened',
      received_at: new Date(Date.UTC(2026, 9, 21, 9, index % 60, 0)).toISOString(),
    });
  }
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const listed = (held: Awaited<ReturnType<typeof world>>, query = '') =>
  held.app.request(`${MOUNT}/workspaces/mine/forge/deliveries${query}`, as(held.key));

describe('what the delivery log takes', () => {
  it('answers thirty rows when no limit is given', async () => {
    const held = await world();

    const rows = (await (await listed(held)).json()) as unknown[];

    expect(rows).toHaveLength(30);
  });

  it('answers as many as the caller asked for', async () => {
    const held = await world();

    expect((await (await listed(held, '?limit=1')).json()) as unknown[]).toHaveLength(1);
    expect((await (await listed(held, '?limit=100')).json()) as unknown[]).toHaveLength(40);
  });

  it.each([
    ['a limit of zero', '?limit=0', 'limit'],
    ['a limit past a hundred', '?limit=101', 'limit'],
    ['a limit that is no number', '?limit=plenty', 'limit'],
    ['a since that is no time', '?since=yesterday', 'since'],
    ['an empty since', '?since=', 'since'],
    ['a repeated since', '?since=2026-10-21T09:00:00Z&since=2026-10-21T10:00:00Z', 'since'],
    ['a repeated limit', '?limit=1&limit=2', 'limit'],
  ])('refuses %s, naming the parameter', async (_what, query, named) => {
    const held = await world(1);

    const response = await listed(held, query);
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message.startsWith(named)).toBe(true);
  });

  it('keeps whole seconds of a time bound, so a fraction starts at the next second', async () => {
    const held = await world(0);
    for (const at of ['2026-10-21T09:00:00.000Z', '2026-10-21T09:00:01.000Z']) {
      await recordDelivery(held.store, {
        workspace_slug: 'mine',
        delivery_id: at,
        forge: 'github',
        event: 'issues',
        received_at: at,
      });
    }

    const rows = (await (await listed(held, '?since=2026-10-21T09:00:00.500Z')).json()) as Array<{
      delivery_id: string;
    }>;

    expect(rows.map((one) => one.delivery_id)).toEqual(['2026-10-21T09:00:01.000Z']);
  });

  it('answers an empty array where nothing has arrived', async () => {
    const held = await world(0);

    expect((await (await listed(held)).json()) as unknown[]).toEqual([]);
  });
});
