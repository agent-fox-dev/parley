import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { REGISTERED_SCOPES } from '../auth/scopes.js';
import { seedApiKey, seedOrg, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

describe('who may administer organizations', () => {
  it.each([
    ['the deployment admin token', 'admin-token', 200],
    ['an API key of a user with the admin role', 'adminKey', 200],
    ['an API key of an ordinary user', 'ordinaryKey', 403],
    ['a token holding every registered scope', 'everyScopePat', 403],
    ['no credential at all', 'none', 401],
  ] as const)('answers %s with %i', async (_name, who, expected) => {
    service = await identityService();
    const { app, store } = service;
    const admin = await seedUser(store, { role: 'admin' });
    const ordinary = await seedUser(store);
    const held = {
      adminKey: (await seedApiKey(store, admin)).presented,
      ordinaryKey: (await seedApiKey(store, ordinary)).presented,
      everyScopePat: (await seedPat(store, ordinary, REGISTERED_SCOPES)).presented,
    };
    const credential = who === 'admin-token' ? ADMIN_TOKEN : who === 'none' ? undefined : held[who];

    const response = await app.request(
      `${MOUNT}/orgs`,
      credential === undefined ? {} : as(credential),
    );

    expect(response.status).toBe(expected);
  });
});

async function refusedCreate(body: Record<string, unknown>) {
  service = await identityService();
  const { app, store } = service;
  const owner = await seedUser(store);
  const existing = await seedOrg(store, owner, { name: 'taken-name', slug: 'taken-slug' });

  const response = await app.request(
    `${MOUNT}/orgs`,
    withJson('POST', { owner_id: owner.id, url: '', ...body }, ADMIN_TOKEN),
  );
  return { status: response.status, existing, store };
}

describe('creating an organization the service refuses', () => {
  it('answers 409 for a name that already exists', async () => {
    const { status } = await refusedCreate({ name: 'taken-name', slug: 'a-free-slug' });

    expect(status).toBe(409);
  });

  it('answers 409 for a slug that already exists', async () => {
    const { status } = await refusedCreate({ name: 'a-free-name', slug: 'taken-slug' });

    expect(status).toBe(409);
  });

  it.each([
    ['an uppercase slug', 'Not-Lower'],
    ['a slug with a space', 'two words'],
    ['a slug with an underscore', 'under_score'],
    ['a slug starting with a hyphen', '-leading'],
    ['an empty slug', ''],
  ])('answers 400 for %s and creates nothing', async (_name, slug) => {
    const { status, store } = await refusedCreate({ name: 'a-free-name', slug });

    expect(status).toBe(400);
    expect(await store.all('SELECT id FROM orgs WHERE name = ?', ['a-free-name'])).toEqual([]);
  });
});
