import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import {
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { storeLocks } from '../platform/store_lock.js';
import { movableClock } from '../platform/local/clock.js';
import { SEED_NOW } from '../testutil/identity.js';
import { workspaceLockKey } from './lock.js';
import type { WorkspaceStatus } from '../store/workspaces.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function workspaces() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  const make = (slug: string, status: WorkspaceStatus) =>
    seedWorkspace(store, { slug, owner_id: owner.id, org_id: org.id, status });

  await make('ready-one', 'ready');
  await make('already-archived', 'archived');
  await make('still-cloning', 'cloning');
  await make('busy-one', 'ready');
  await make('pending-one', 'pending');

  return { ...service, owner, key: (await seedApiKey(store, owner)).presented };
}

const statusOf = async (held: Awaited<ReturnType<typeof workspaces>>, slug: string) =>
  (
    await held.store.all<{ status: string }>('SELECT status FROM workspaces WHERE slug = ?', [slug])
  )[0]?.status;

const post = (held: Awaited<ReturnType<typeof workspaces>>, slug: string, action: string) =>
  held.app.request(`${MOUNT}/workspaces/${slug}/${action}`, as(held.key, { method: 'POST' }));

describe('archiving a workspace the service refuses', () => {
  it('answers 400 for one already archived, leaving its status alone', async () => {
    const held = await workspaces();

    const response = await post(held, 'already-archived', 'archive');

    expect(response.status).toBe(400);
    expect(await statusOf(held, 'already-archived')).toBe('archived');
  });

  it('answers 409 for one still cloning', async () => {
    const held = await workspaces();

    const response = await post(held, 'still-cloning', 'archive');

    expect(response.status).toBe(409);
    expect(await statusOf(held, 'still-cloning')).toBe('cloning');
  });

  it('answers 409 workspace_busy while another operation holds the lock', async () => {
    const held = await workspaces();
    const locks = storeLocks(held.store, movableClock(SEED_NOW).clock);
    const holding = await locks.take(workspaceLockKey('busy-one'));
    expect(holding).toBeDefined();

    const response = await post(held, 'busy-one', 'archive');
    const body = (await response.json()) as { error: { error_type?: string } };

    expect(response.status).toBe(409);
    expect(body.error.error_type).toBe('workspace_busy');
    expect(await statusOf(held, 'busy-one')).toBe('ready');

    await holding?.release();
    expect((await post(held, 'busy-one', 'archive')).status).toBe(200);
  });
});

describe('reactivating a workspace that is not archived', () => {
  it.each(['ready-one', 'still-cloning', 'pending-one'])('answers 409 for %s', async (slug) => {
    const held = await workspaces();
    const before = await statusOf(held, slug);

    const response = await post(held, slug, 'reactivate');

    expect(response.status).toBe(409);
    expect(await statusOf(held, slug)).toBe(before);
  });
});

describe('deleting a workspace that is not archived', () => {
  it.each(['ready-one', 'pending-one'])('answers 409 for %s and keeps everything', async (slug) => {
    const held = await workspaces();
    const token = await seedBoundToken(held.store, held.owner, slug, ['workspaces:read']);

    const response = await held.app.request(
      `${MOUNT}/workspaces/${slug}`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(409);
    expect(await statusOf(held, slug)).toBeDefined();
    expect(
      (await held.app.request(`${MOUNT}/workspaces/${slug}`, as(token.presented))).status,
    ).toBe(200);
  });
});
