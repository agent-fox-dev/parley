import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser } from '../testutil/identity.js';
import { REGISTERED_SCOPES } from '../auth/scopes.js';
import { findUser } from '../store/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function callers() {
  service = await identityService();
  const { store } = service;
  const admin = await seedUser(store, { role: 'admin' });
  const ordinary = await seedUser(store);
  return {
    ...service,
    subject: ordinary,
    adminKey: (await seedApiKey(store, admin)).presented,
    ordinaryKey: (await seedApiKey(store, ordinary)).presented,
    everyScopePat: (await seedPat(store, ordinary, REGISTERED_SCOPES)).presented,
  };
}

describe('who may administer users', () => {
  it.each([
    ['the deployment admin token', 'admin-token', 200],
    ['an API key of a user with the admin role', 'adminKey', 200],
    ['an API key of an ordinary user', 'ordinaryKey', 403],
    ['a token holding every registered scope', 'everyScopePat', 403],
    ['no credential at all', 'none', 401],
  ] as const)('answers %s with %i', async (_name, who, expected) => {
    const held = await callers();
    const credential = who === 'admin-token' ? ADMIN_TOKEN : who === 'none' ? undefined : held[who];

    const response = await held.app.request(
      `${MOUNT}/users`,
      credential === undefined ? {} : as(credential),
    );

    expect(response.status).toBe(expected);
  });
});

describe('the last active admin', () => {
  it.each([
    ['demoted', 'demote'],
    ['deleted', 'delete'],
  ])('cannot be %s while no other admin is active', async (_name, action) => {
    service = await identityService();
    const { app, store } = service;
    const only = await seedUser(store, { role: 'admin' });

    const response =
      action === 'demote'
        ? await app.request(`${MOUNT}/users/${only.id}/demote`, as(ADMIN_TOKEN, { method: 'POST' }))
        : await app.request(`${MOUNT}/users/${only.id}`, as(ADMIN_TOKEN, { method: 'DELETE' }));

    expect(response.status).toBe(409);
    const after = await findUser(store, only.id);
    expect(after?.role).toBe('admin');
    expect(after?.status).toBe('active');
  });

  it('may be demoted once a second admin is active', async () => {
    service = await identityService();
    const { app, store } = service;
    const first = await seedUser(store, { role: 'admin' });
    await seedUser(store, { role: 'admin' });

    const response = await app.request(
      `${MOUNT}/users/${first.id}/demote`,
      as(ADMIN_TOKEN, { method: 'POST' }),
    );

    expect(response.status).toBe(200);
    expect((await findUser(store, first.id))?.role).toBe('user');
  });

  it('does not count a blocked admin as active', async () => {
    service = await identityService();
    const { app, store } = service;
    const active = await seedUser(store, { role: 'admin' });
    await seedUser(store, { role: 'admin', status: 'blocked' });

    const response = await app.request(
      `${MOUNT}/users/${active.id}/demote`,
      as(ADMIN_TOKEN, { method: 'POST' }),
    );

    expect(response.status).toBe(409);
  });
});

describe('creating a user as an administrator', () => {
  it.each([
    ['a username that already exists', 'username'],
    ['a provider identity that already exists', 'identity'],
  ])('answers 409 for %s', async (_name, which) => {
    service = await identityService();
    const { app, store } = service;
    const existing = await seedUser(store);

    const body =
      which === 'username'
        ? {
            username: existing.username,
            email: 'other@example.com',
            provider: 'github',
            provider_id: 'something-else',
          }
        : {
            username: 'a-new-name',
            email: 'other@example.com',
            provider: existing.provider,
            provider_id: existing.provider_id,
          };

    const response = await app.request(`${MOUNT}/users`, withJson('POST', body, ADMIN_TOKEN));

    expect(response.status).toBe(409);
  });

  it.each([
    ['no username', { email: 'a@example.com', provider: 'github', provider_id: '1' }],
    ['no email', { username: 'a', provider: 'github', provider_id: '1' }],
    ['no provider', { username: 'a', email: 'a@example.com', provider_id: '1' }],
    ['no provider_id', { username: 'a', email: 'a@example.com', provider: 'github' }],
  ])('answers 400 for %s', async (_name, body) => {
    service = await identityService();

    const response = await service.app.request(
      `${MOUNT}/users`,
      withJson('POST', body, ADMIN_TOKEN),
    );

    expect(response.status).toBe(400);
  });
});
