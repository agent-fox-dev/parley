import { afterEach, describe, expect, it } from 'vitest';
import { type Service, identityService } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What happens to a running agent when its workspace is retired.
 * The session cannot report that it finished, so the hub records
 * that it was ended — and the agent's next read is refused, because
 * the workspace is no longer active.
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
  other: string;
  credential: string;
}

async function world(): Promise<World> {
  const service = await identityService([], { clock: 'ticking' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const made = async () =>
    (await seedWorkspace(service.store, { owner_id: owner.id, org_id: org.id, status: 'ready' }))
      .slug;

  return {
    service,
    slug: await made(),
    other: await made(),
    credential: (await seedApiKey(service.store, owner)).presented,
  };
}

async function send(place: World, path: string, body?: unknown): Promise<Response> {
  return place.service.app.request(`/api/v1${path}`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${place.credential}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function statusOf(place: World, id: string): Promise<string> {
  const rows = await place.service.store.all<{ status: string }>(
    'SELECT status FROM sessions WHERE id = ?',
    [id],
  );
  return rows[0]?.status ?? '';
}

describe('a workspace retired under a running agent', () => {
  it('ends its sessions, and leaves another workspace alone', async () => {
    const place = await world();
    const here = (
      (await (await send(place, '/sessions', { workspace_slug: place.slug })).json()) as {
        id: string;
      }
    ).id;
    const elsewhere = (
      (await (await send(place, '/sessions', { workspace_slug: place.other })).json()) as {
        id: string;
      }
    ).id;
    const closed = (
      (await (await send(place, '/sessions', { workspace_slug: place.slug })).json()) as {
        id: string;
      }
    ).id;
    await send(place, `/sessions/${closed}/complete`, { status: 'completed' });

    const archived = await send(place, `/workspaces/${place.slug}/archive`);

    expect(archived.status).toBe(200);
    expect(await statusOf(place, here)).toBe('terminated');
    // A session that had already finished keeps how it finished.
    expect(await statusOf(place, closed)).toBe('completed');
    // And another workspace's agent is untouched.
    expect(await statusOf(place, elsewhere)).toBe('active');

    const left = await place.service.store.all(
      "SELECT id FROM sessions WHERE workspace_slug = ? AND status = 'active'",
      [place.slug],
    );
    expect(left).toEqual([]);
  });

  it("refuses the agent's next read, because the workspace is no longer active", async () => {
    const place = await world();
    const id = (
      (await (await send(place, '/sessions', { workspace_slug: place.slug })).json()) as {
        id: string;
      }
    ).id;
    await send(place, `/workspaces/${place.slug}/archive`);

    const read = await place.service.app.request(`/api/v1/sessions/${id}`, {
      headers: { authorization: `Bearer ${place.credential}` },
    });

    expect(read.status).toBe(403);
    expect(((await read.json()) as { error: { message: string } }).error.message).toContain(
      'workspace access denied',
    );
  });

  it('ends them when the workspace is deleted for good', async () => {
    const place = await world();
    const id = (
      (await (await send(place, '/sessions', { workspace_slug: place.slug })).json()) as {
        id: string;
      }
    ).id;
    await send(place, `/workspaces/${place.slug}/archive`);
    await place.service.store.run("UPDATE sessions SET status = 'active' WHERE id = ?", [id]);

    const deleted = await place.service.app.request(`/api/v1/workspaces/${place.slug}`, {
      method: 'DELETE',
      headers: { authorization: `Bearer ${place.credential}` },
    });

    expect(deleted.status).toBe(204);
    expect(await statusOf(place, id)).toBe('terminated');
  });
});
