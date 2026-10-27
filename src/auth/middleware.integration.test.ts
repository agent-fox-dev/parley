import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import type { RouteDefinition } from '../http/router.js';
import { findCredential } from '../store/identity.js';
import { freshStore, seedApiKey, seedClock, seedPat, seedUser } from '../testutil/identity.js';

const MOUNT = '/api/v1';
const PROTECTED: RouteDefinition[] = [
  { path: `${MOUNT}/protected`, handler: () => new Response('{}', { status: 200 }) },
];

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

describe('a credential whose user is blocked', () => {
  it('stops working while the block lasts, without the credential being revoked', async () => {
    const seeded = await freshStore();
    disposers.push(seeded.dispose);
    const { store } = seeded;

    const user = await seedUser(store);
    const key = await seedApiKey(store, user);
    const pat = await seedPat(store, user, ['workspaces:read']);
    const app = buildApp({
      routes: PROTECTED,
      store,
      clock: seedClock(),
      auth: { store, clock: seedClock(), mountPoint: MOUNT },
    });

    const call = async (credential: string) =>
      (
        await app.request(`${MOUNT}/protected`, {
          headers: { authorization: `Bearer ${credential}` },
        })
      ).status;

    expect(await call(key.presented)).toBe(200);
    expect(await call(pat.presented)).toBe(200);

    await store.run('UPDATE users SET status = ? WHERE id = ?', ['blocked', user.id]);

    expect(await call(key.presented)).toBe(401);
    expect(await call(pat.presented)).toBe(401);
    expect((await findCredential(store, 'api_key', key.record.id))?.revoked_at).toBeNull();
    expect((await findCredential(store, 'pat', pat.record.id))?.revoked_at).toBeNull();

    await store.run('UPDATE users SET status = ? WHERE id = ?', ['active', user.id]);

    expect(await call(key.presented)).toBe(200);
    expect(await call(pat.presented)).toBe(200);
  });
});
