import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';
import { pruneDeliveries } from '../jobs/delivery_prune.js';
import { recordDelivery } from '../webhook/log.js';

/**
 * The log of what the forge sent. It is what an operator reads when
 * a delivery did not do what they expected, so it says what the hub
 * made of each one — and holds none of the payload, which may carry
 * anything at all.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

const SECRET_IN_PAYLOAD = 'a-string-from-the-payload';

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });

  await recordDelivery(service.store, {
    workspace_slug: 'mine',
    delivery_id: 'd-relayed',
    forge: 'github',
    event: 'issues',
    action: 'labeled',
    target: 'origin',
    outcome: 'relayed',
    relayed: ['hub.forge.issue.labeled'],
    received_at: '2026-10-21T09:00:00.000Z',
  });
  // Two in the same second: the later recording comes first.
  for (const id of ['d-ignored', 'd-mismatch']) {
    await recordDelivery(service.store, {
      workspace_slug: 'mine',
      delivery_id: id,
      forge: 'gitlab',
      event: 'Push Hook',
      outcome: id === 'd-ignored' ? 'ignored' : 'repository_mismatch',
      received_at: '2026-10-21T09:00:05.000Z',
    });
  }
  await recordDelivery(service.store, {
    workspace_slug: 'mine',
    delivery_id: 'd-malformed',
    forge: 'github',
    event: 'issues',
    outcome: 'malformed',
    received_at: '2026-10-21T09:00:09.000Z',
    // Whatever a caller passes, no payload is kept.
    action: SECRET_IN_PAYLOAD.slice(0, 0),
  });

  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

describe('the delivery log', () => {
  it('lists what arrived, newest first, with the same second in recording order', async () => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/forge/deliveries`,
      as(held.key),
    );
    const rows = (await response.json()) as Array<Record<string, unknown>>;

    expect(response.status).toBe(200);
    expect(rows.map((one) => one['delivery_id'])).toEqual([
      'd-malformed',
      'd-mismatch',
      'd-ignored',
      'd-relayed',
    ]);
    expect(Object.keys(rows[0] ?? {}).toSorted()).toEqual([
      'action',
      'delivery_id',
      'event',
      'forge',
      'outcome',
      'received_at',
      'relayed',
      'target',
    ]);
  });

  it('carries the relayed events for a relayed delivery and none for the rest', async () => {
    const held = await world();

    const rows = (await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge/deliveries`, as(held.key))
    ).json()) as Array<{ delivery_id: string; relayed: string[]; target: string | null }>;

    const byId = new Map(rows.map((one) => [one.delivery_id, one]));
    expect(byId.get('d-relayed')?.relayed).toEqual(['hub.forge.issue.labeled']);
    expect(byId.get('d-ignored')?.relayed).toEqual([]);
    expect(byId.get('d-mismatch')?.relayed).toEqual([]);
    expect(byId.get('d-ignored')?.target).toBeNull();
  });

  it('holds nothing of the payload', async () => {
    const held = await world();

    const text = await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge/deliveries`, as(held.key))
    ).text();

    expect(text).not.toContain(SECRET_IN_PAYLOAD);
  });
});

describe('what the hourly prune removes', () => {
  it('takes deliveries past seven days and those of a workspace that is gone', async () => {
    const held = await world();
    const now = new Date(SEED_NOW.getTime());
    const moving = movableClock(now);
    const ago = (days: number, seconds = 0) =>
      new Date(now.getTime() - days * 86_400_000 - seconds * 1000).toISOString();

    await seedWorkspace(held.store, {
      slug: 'gone',
      owner_id: 'u-1',
      status: 'ready',
    });
    for (const [id, slug, at] of [
      ['six-days', 'mine', ago(6)],
      ['seven-days-and-a-second', 'mine', ago(7, 1)],
      ['thirty-days', 'mine', ago(30)],
      ['of-a-gone-workspace', 'gone', ago(0)],
    ] as const) {
      await recordDelivery(held.store, {
        workspace_slug: slug,
        delivery_id: id,
        forge: 'github',
        event: 'issues',
        received_at: at,
      });
    }
    await held.store.run('DELETE FROM workspaces WHERE slug = ?', ['gone']);

    await pruneDeliveries(held.store, moving.clock);

    const left = await held.store.all<{ delivery_id: string }>(
      'SELECT delivery_id FROM forge_deliveries',
    );
    const ids = left.map((one) => one.delivery_id);
    expect(ids).toContain('six-days');
    expect(ids).not.toContain('seven-days-and-a-second');
    expect(ids).not.toContain('thirty-days');
    expect(ids).not.toContain('of-a-gone-workspace');
  });
});

describe('the webhook block of the forge description', () => {
  it('reports the latest delivery this log holds', async () => {
    const held = await world();

    const described = (await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge`, as(held.key))
    ).json()) as { webhook: { last_delivery_at: string | null } };

    expect(described.webhook.last_delivery_at).toBe('2026-10-21T09:00:09.000Z');
  });
});
