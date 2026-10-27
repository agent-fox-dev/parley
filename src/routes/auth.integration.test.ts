import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { findCredential, orgsOfUser } from '../store/identity.js';
import { freshStore, seedApiKey, seedClock, seedUser } from '../testutil/identity.js';
import { providerIdentity, stubDirectory } from '../testutil/oauth.js';
import { authRoutes } from './auth.js';

const MOUNT = '/api/v1';
const ALLOWED_REDIRECT = 'http://localhost:8080/callback';

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

interface Fixture {
  app: ReturnType<typeof buildApp>;
  store: RelationalStore;
}

async function signInService(
  codes: Parameters<typeof stubDirectory>[0]['codes'],
): Promise<Fixture> {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  const app = buildApp({
    routes: [
      ...authRoutes({
        mountPoint: MOUNT,
        store: seeded.store,
        clock: seedClock(),
        providers: stubDirectory({ codes, redirectAllowlist: [ALLOWED_REDIRECT] }),
      }),
      { path: `${MOUNT}/protected`, handler: () => new Response('{}', { status: 200 }) },
    ],
    store: seeded.store,
    clock: seedClock(),
    auth: { store: seeded.store, clock: seedClock(), mountPoint: MOUNT },
  });
  return { app, store: seeded.store };
}

const callback = (body: unknown) => ({
  method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify(body),
});

async function callWith(app: Fixture['app'], credential: string): Promise<number> {
  const response = await app.request(`${MOUNT}/protected`, {
    headers: { authorization: `Bearer ${credential}` },
  });
  return response.status;
}

interface CallbackBody {
  user: { id: string; username: string; status: string; role: string };
  api_key: { key: string; key_id: string; expires_at: string | null };
}

describe('signing in', () => {
  it('creates a user with a personal organization and returns a usable key', async () => {
    const { app, store } = await signInService({ fresh: providerIdentity({ username: 'alice' }) });

    const response = await app.request(
      `${MOUNT}/auth/callback`,
      callback({ provider: 'github', code: 'fresh' }),
    );
    const body = (await response.json()) as CallbackBody;

    expect(response.status).toBe(200);
    expect(body.user.username).toBe('alice');
    expect(body.api_key.key).toMatch(/^af_[0-9a-f]+_[0-9a-f]+$/);
    expect(body.api_key.key.startsWith(`af_${body.api_key.key_id}_`)).toBe(true);
    expect(await callWith(app, body.api_key.key)).toBe(200);

    const orgs = await orgsOfUser(store, body.user.id);
    expect(orgs).toHaveLength(1);
    expect(orgs[0]?.owner_id).toBe(body.user.id);
  });

  it('revokes every key the user already held', async () => {
    const identity = providerIdentity({ username: 'bob', provider_id: 'bob-1' });
    const { app, store } = await signInService({ again: identity });
    const user = await seedUser(store, {
      username: identity.username,
      provider: identity.provider,
      provider_id: identity.provider_id,
    });
    const first = await seedApiKey(store, user);
    const second = await seedApiKey(store, user);

    expect(await callWith(app, first.presented)).toBe(200);

    const response = await app.request(
      `${MOUNT}/auth/callback`,
      callback({ provider: 'github', code: 'again' }),
    );
    const body = (await response.json()) as CallbackBody;

    expect(response.status).toBe(200);
    expect(await callWith(app, first.presented)).toBe(401);
    expect(await callWith(app, second.presented)).toBe(401);
    expect(await callWith(app, body.api_key.key)).toBe(200);
    expect((await findCredential(store, 'api_key', first.record.id))?.revoked_at).not.toBeNull();
  });

  it('does not create a second organization for a user signing in again', async () => {
    const identity = providerIdentity({ username: 'carol', provider_id: 'carol-1' });
    const { app, store } = await signInService({ twice: identity });

    const first = (await (
      await app.request(`${MOUNT}/auth/callback`, callback({ provider: 'github', code: 'twice' }))
    ).json()) as CallbackBody;
    await app.request(`${MOUNT}/auth/callback`, callback({ provider: 'github', code: 'twice' }));

    expect(await orgsOfUser(store, first.user.id)).toHaveLength(1);
  });
});

async function refused(body: unknown, prepare?: (store: RelationalStore) => Promise<void>) {
  const { app, store } = await signInService({
    good: providerIdentity({ username: 'dave', provider_id: 'dave-1' }),
    blocked: providerIdentity({ username: 'erin', provider_id: 'erin-1' }),
    conflicting: providerIdentity({ username: 'frank', provider_id: 'other-id' }),
  });
  await prepare?.(store);

  const before = {
    users: await store.all('SELECT id, status FROM users ORDER BY id'),
    credentials: await store.all('SELECT id, revoked_at FROM credentials ORDER BY id'),
  };
  const response = await app.request(`${MOUNT}/auth/callback`, callback(body));
  const after = {
    users: await store.all('SELECT id, status FROM users ORDER BY id'),
    credentials: await store.all('SELECT id, revoked_at FROM credentials ORDER BY id'),
  };

  return { status: response.status, before, after };
}

describe('a sign-in the service refuses', () => {
  it('answers 401 for a code the provider does not accept', async () => {
    const { status, before, after } = await refused({ provider: 'github', code: 'nonsense' });

    expect(status).toBe(401);
    expect(after).toEqual(before);
  });

  it('answers 400 for a redirect_uri outside the provider allowlist', async () => {
    const { status, before, after } = await refused({
      provider: 'github',
      code: 'good',
      redirect_uri: 'https://evil.invalid/steal',
    });

    expect(status).toBe(400);
    expect(after).toEqual(before);
  });

  it('answers 403 for a blocked user, revoking nothing', async () => {
    const { status, before, after } = await refused(
      { provider: 'github', code: 'blocked' },
      async (store) => {
        const user = await seedUser(store, {
          username: 'erin',
          provider: 'github',
          provider_id: 'erin-1',
          status: 'blocked',
        });
        await seedApiKey(store, user);
      },
    );

    expect(status).toBe(403);
    expect(after).toEqual(before);
  });

  it('answers 409 when the username belongs to another provider identity', async () => {
    const { status, before, after } = await refused(
      { provider: 'github', code: 'conflicting' },
      async (store) => {
        await seedUser(store, {
          username: 'frank',
          provider: 'github',
          provider_id: 'frank-original',
        });
      },
    );

    expect(status).toBe(409);
    expect(after).toEqual(before);
  });

  it.each([
    ['an unknown provider', { provider: 'gitlab', code: 'good' }],
    ['no provider', { code: 'good' }],
    ['no code', { provider: 'github' }],
    ['a body that is not an object', 'not-an-object'],
  ])('answers 400 for %s', async (_name, body) => {
    const { status, before, after } = await refused(body);

    expect(status).toBe(400);
    expect(after).toEqual(before);
  });
});
