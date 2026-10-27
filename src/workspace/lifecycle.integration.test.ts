import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import {
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { workspaceScope } from '../platform/secrets.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function retiring() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  await seedWorkspace(store, {
    slug: 'the-project',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  const key = (await seedApiKey(store, owner)).presented;
  const bound = await seedBoundToken(store, owner, 'the-project', ['workspaces:read']);
  const second = await seedBoundToken(store, owner, 'the-project', ['workspaces:read']);
  await service.secrets.put(workspaceScope('the-project'), 'GIT_PAT', 'ghp_SENTINEL');

  return { ...service, owner, key, bound, second };
}

const post = (held: Awaited<ReturnType<typeof retiring>>, action: string) =>
  held.app.request(`${MOUNT}/workspaces/the-project/${action}`, as(held.key, { method: 'POST' }));

const reachable = async (held: Awaited<ReturnType<typeof retiring>>, credential: string) =>
  (await held.app.request(`${MOUNT}/workspaces/the-project`, as(credential))).status;

describe('archiving a workspace', () => {
  it('answers with it archived, and leaves its bound tokens working', async () => {
    const held = await retiring();

    const response = await post(held, 'archive');
    const body = (await response.json()) as { status: string };

    expect(response.status).toBe(200);
    expect(body.status).toBe('archived');
    expect(await reachable(held, held.bound.presented)).toBe(200);
    expect(await reachable(held, held.key)).toBe(200);
  });

  it('releases the lock, so the workspace can be archived again after a reactivation', async () => {
    const held = await retiring();

    await post(held, 'archive');
    expect((await post(held, 'reactivate')).status).toBe(200);
    expect((await post(held, 'archive')).status).toBe(200);
  });
});

describe('reactivating an archived workspace', () => {
  it('returns it to pending, and a later read says so', async () => {
    const held = await retiring();
    await post(held, 'archive');

    const response = await post(held, 'reactivate');

    expect(response.status).toBe(200);
    expect(((await response.json()) as { status: string }).status).toBe('pending');

    const read = await held.app.request(`${MOUNT}/workspaces/the-project`, as(held.key));
    expect(((await read.json()) as { status: string }).status).toBe('pending');
  });
});

describe('deleting an archived workspace', () => {
  it('takes its bound tokens and its secrets with it', async () => {
    const held = await retiring();
    await post(held, 'archive');

    const response = await held.app.request(
      `${MOUNT}/workspaces/the-project`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    expect(await reachable(held, held.key)).toBe(404);
    expect(await reachable(held, held.bound.presented)).toBe(401);
    expect(await reachable(held, held.second.presented)).toBe(401);
    expect(await held.secrets.names(workspaceScope('the-project'))).toEqual([]);

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces?include_archived=true`, as(held.key))
    ).json()) as Array<{ slug: string }>;
    expect(listed.map((w) => w.slug)).not.toContain('the-project');
  });

  it('leaves the workspace and its tokens untouched when the transaction cannot finish', async () => {
    const held = await retiring();
    await post(held, 'archive');
    // The bindings table is gone, so one statement of the delete fails and
    // the whole transaction must leave nothing changed.
    await held.store.run('DROP TABLE token_bindings');

    const response = await held.app.request(
      `${MOUNT}/workspaces/the-project`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(500);
    expect(
      await held.store.all('SELECT slug FROM workspaces WHERE slug = ?', ['the-project']),
    ).toHaveLength(1);
    expect(
      (
        await held.store.all<{ revoked_at: string | null }>(
          'SELECT revoked_at FROM credentials WHERE id = ?',
          [held.bound.record.id],
        )
      )[0]?.revoked_at,
    ).toBeNull();
  });
});
