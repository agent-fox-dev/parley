import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { SEED_NOW, freshStore, seedClock } from '../testutil/identity.js';
import { providerIdentity, stubDirectory } from '../testutil/oauth.js';
import { authRoutes } from './auth.js';

const MOUNT = '/api/v1';
const GOOD_CODE = 'code-that-works';

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

async function appWithProviders(allowlist: readonly string[] = []) {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  const providers = stubDirectory({
    codes: { [GOOD_CODE]: providerIdentity() },
    redirectAllowlist: allowlist,
    clientId: 'client-abc',
  });

  const app = buildApp({
    routes: authRoutes({
      mountPoint: MOUNT,
      store: seeded.store,
      clock: seedClock(),
      providers,
    }),
    store: seeded.store,
    clock: seedClock(),
    auth: { store: seeded.store, clock: seedClock(), mountPoint: MOUNT },
  });

  return { app, store: seeded.store as RelationalStore };
}

const callback = (body: unknown) => ({
  method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify(body),
});

describe('the provider list', () => {
  it('names each configured provider with its authorize URL', async () => {
    const { app } = await appWithProviders();

    const response = await app.request(`${MOUNT}/auth/providers`);
    const body = (await response.json()) as Array<{ name: string; authorize_url: string }>;

    expect(response.status).toBe(200);
    expect(body).toHaveLength(1);
    expect(body[0]?.name).toBe('github');
    expect(body[0]?.authorize_url).toContain('client-abc');
  });

  it('may be cached publicly for five minutes', async () => {
    const { app } = await appWithProviders();

    const header = (await app.request(`${MOUNT}/auth/providers`)).headers.get('cache-control');

    expect(header).toContain('public');
    expect(header).toContain('max-age=300');
  });

  it('is served without a credential', async () => {
    const { app } = await appWithProviders();

    expect((await app.request(`${MOUNT}/auth/providers`)).status).toBe(200);
  });
});

describe('the lifetime a new key is issued with', () => {
  const day = 24 * 60 * 60 * 1000;
  const ahead = (days: number) => new Date(SEED_NOW.getTime() + days * day).toISOString();

  it.each([
    ['0, which never expires', 0, null],
    ['30 days', 30, ahead(30)],
    ['60 days', 60, ahead(60)],
    ['90 days', 90, ahead(90)],
  ])('is %s', async (_name, expires, expected) => {
    const { app } = await appWithProviders();

    const response = await app.request(
      `${MOUNT}/auth/callback`,
      callback({ provider: 'github', code: GOOD_CODE, expires }),
    );
    const body = (await response.json()) as { api_key: { expires_at: string | null } };

    expect(response.status).toBe(200);
    expect(body.api_key.expires_at).toBe(expected);
  });

  it('is ninety days when the caller asks for none', async () => {
    const { app } = await appWithProviders();

    const response = await app.request(
      `${MOUNT}/auth/callback`,
      callback({ provider: 'github', code: GOOD_CODE }),
    );
    const body = (await response.json()) as { api_key: { expires_at: string | null } };

    expect(body.api_key.expires_at).toBe(ahead(90));
  });

  it.each([
    ['45 days', 45],
    ['a negative lifetime', -1],
    ['a fractional lifetime', 1.5],
    ['a lifetime that is not a number', 'ninety'],
  ])('refuses %s, issuing no key', async (_name, expires) => {
    const { app, store } = await appWithProviders();

    const response = await app.request(
      `${MOUNT}/auth/callback`,
      callback({ provider: 'github', code: GOOD_CODE, expires }),
    );

    expect(response.status).toBe(400);
    expect(await store.all('SELECT id FROM credentials')).toEqual([]);
  });
});
