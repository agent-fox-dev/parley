import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The transcript names a node, because a run's traces are several
 * conversations interleaved and a transcript of all of them at once
 * would be nobody's conversation.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const path = (query: string) => `${MOUNT}/workspaces/mine/runs/${RUN}/transcript${query}`;

describe('a transcript that names no node', () => {
  it.each([
    ['nothing at all', ''],
    ['an empty node', '?node_id='],
  ])('is refused when it asks with %s', async (_what, query) => {
    const held = await world();

    const response = await held.app.request(path(query), as(held.key));

    expect(response.status).toBe(400);
  });

  it('is refused for a run id of the wrong shape', async () => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/mine/runs/nonsense/transcript?node_id=n-1`,
      as(held.key),
    );

    expect(response.status).toBe(400);
  });

  it('answers an empty conversation for a node with no traces', async () => {
    const held = await world();

    const response = await held.app.request(path('?node_id=n-1'), as(held.key));
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(body).toEqual({ run_id: RUN, node_id: 'n-1', messages: [] });
  });
});
