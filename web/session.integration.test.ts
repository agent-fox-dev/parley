import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../src/testutil/app.js';
import { seedApiKey, seedClock, seedPat, seedUser } from '../src/testutil/identity.js';
import { SESSION_COOKIE, signedInUserId } from './session.js';

/**
 * Signing in through the browser. The whole reason this package has
 * a session of its own is here: the API's sign-in revokes every API
 * key the person holds, and a developer opening the dashboard must
 * not kill the key their agent is running with.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  return service;
}

/** Signs in with a code and answers with the session cookie's value. */
async function signIn(held: Service, code: string): Promise<string> {
  const response = await held.app.request(`/signin/github/callback?code=${code}`);
  expect(response.status, `the callback answered ${String(response.status)}`).toBe(302);
  const cookie = response.headers.get('set-cookie') ?? '';
  const value = /parley_session=([^;]+)/.exec(cookie)?.[1] ?? '';
  expect(value.length, 'the answer carried a session cookie').toBeGreaterThan(0);
  return value;
}

const withSession = (value: string) => ({ headers: { cookie: `${SESSION_COOKIE}=${value}` } });

describe('signing in through the browser', () => {
  it('sends a visitor to the provider', async () => {
    const held = await world();

    const response = await held.app.request('/signin/github');

    expect(response.status).toBe(302);
    expect(response.headers.get('location') ?? '').toContain('https://provider.invalid/authorize');
  });

  it('establishes a session that identifies the person the provider named', async () => {
    const held = await world();

    const value = await signIn(held, 'a-code');

    const who = await signedInUserId(held.store, seedClock(), value);
    const [user] = await held.store.all<{ username: string }>(
      'SELECT username FROM users WHERE id = ?',
      [who ?? ''],
    );
    expect(user?.username).toBe('alice');
  });

  it('gives the provider the same address it sent them to authorize at', async () => {
    const asked: Array<{ code: string; redirectUri?: string }> = [];
    service = await identityService([], { asked });

    const sent = await service.app.request('/signin/github');
    const authorize = new URL(sent.headers.get('location') ?? 'https://nowhere.invalid');
    const callback = authorize.searchParams.get('redirect_uri') ?? '';
    expect(callback).toMatch(/\/signin\/github\/callback$/);

    await service.app.request('/signin/github/callback?code=a-code');

    // A provider that holds the authorization code to the address it
    // was issued for — which the standard requires of one — refuses
    // an exchange that names none.
    expect(asked).toEqual([{ code: 'a-code', redirectUri: callback }]);
  });

  it('refuses a code the provider does not know, and opens no session', async () => {
    const held = await world();

    const response = await held.app.request('/signin/github/callback?code=not-a-code');

    expect(response.status).toBe(401);
    expect(response.headers.get('set-cookie')).toBeNull();
  });
});

describe('what a browser sign-in does to the person’s API keys', () => {
  it('leaves every one of them working', async () => {
    const held = await world();
    const alice = await seedUser(held.store, { username: 'alice', provider_id: '12345' });
    const first = await seedApiKey(held.store, alice);
    const second = await seedApiKey(held.store, alice);
    const token = await seedPat(held.store, alice, ['workspaces:read']);

    await signIn(held, 'a-code');

    for (const key of [first, second]) {
      const answered = await held.app.request(`${MOUNT}/user`, as(key.presented));
      expect(answered.status, 'the API key still works').toBe(200);
    }
    // The token too, at a route its own scope admits it to.
    expect(
      (await held.app.request(`${MOUNT}/workspaces`, as(token.presented))).status,
      'the personal access token still works',
    ).toBe(200);
    // And no key was issued or revoked on the way.
    const keys = await held.store.all<{ id: string; revoked_at: string | null }>(
      "SELECT id, revoked_at FROM credentials WHERE user_id = ? AND kind = 'api_key'",
      [alice.id],
    );
    expect(keys).toHaveLength(2);
    expect(keys.every((key) => key.revoked_at === null)).toBe(true);
  });
});

describe('a session that should not be honoured', () => {
  it('is not honoured when the request comes from another site', async () => {
    const held = await world();
    const value = await signIn(held, 'a-code');

    const crossSite = await held.app.request('/dashboard', {
      headers: { cookie: `${SESSION_COOKIE}=${value}`, 'sec-fetch-site': 'cross-site' },
    });

    expect(crossSite.status).toBe(200);
    // Served as a visitor, not as the person.
    expect(await crossSite.text()).not.toContain('Sign out');
  });

  it('is not honoured once it has been signed out', async () => {
    const held = await world();
    const value = await signIn(held, 'a-code');

    const out = await held.app.request('/signout', withSession(value));
    const after = await held.app.request('/dashboard', withSession(value));

    expect(out.status).toBe(302);
    expect(out.headers.get('set-cookie') ?? '').toContain('Max-Age=0');
    expect(await after.text()).toContain('Sign in');
  });

  it('shows the landing page rather than an error when there is none', async () => {
    const held = await world();

    const nothing = await held.app.request('/dashboard');
    const nonsense = await held.app.request('/dashboard', withSession('not-a-session'));

    for (const response of [nothing, nonsense]) {
      const html = await response.text();
      expect(response.status).toBe(200);
      expect(html).toContain('Sign in');
      expect(html).not.toMatch(/stack trace|Error:|error_type/i);
    }
  });
});

describe('two people signed in at once', () => {
  it('hold sessions that identify each of them, and not each other', async () => {
    const held = await world();
    const alice = await seedUser(held.store, { username: 'alice', provider_id: '12345' });
    const bob = await seedUser(held.store, { username: 'bob', provider_id: '67890' });

    const hers = await signIn(held, 'a-code');
    const his = await signIn(held, 'b-code');

    expect(await signedInUserId(held.store, seedClock(), hers)).toBe(alice.id);
    expect(await signedInUserId(held.store, seedClock(), his)).toBe(bob.id);
  });
});
