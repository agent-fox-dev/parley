import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser } from '../testutil/identity.js';

const PROFILE_FIELDS = [
  'created_at',
  'email',
  'full_name',
  'id',
  'provider',
  'provider_id',
  'role',
  'status',
  'updated_at',
  'username',
];

let service: Service;

afterEach(() => {
  service.dispose();
});

async function signedIn() {
  service = await identityService();
  const user = await seedUser(service.store, { full_name: 'Alice Example' });
  const key = await seedApiKey(service.store, user);
  return { ...service, user, key: key.presented };
}

describe("the caller's profile", () => {
  it('carries exactly the documented fields', async () => {
    const { app, key } = await signedIn();

    const response = await app.request(`${MOUNT}/user`, as(key));
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(Object.keys(body).toSorted()).toEqual(PROFILE_FIELDS);
  });

  it('changes the display name and nothing else', async () => {
    const { app, key, user } = await signedIn();

    const updated = await app.request(
      `${MOUNT}/user`,
      withJson('PATCH', { full_name: 'Alice Renamed' }, key),
    );
    const after = (await (await app.request(`${MOUNT}/user`, as(key))).json()) as Record<
      string,
      unknown
    >;

    expect(updated.status).toBe(200);
    expect(after['full_name']).toBe('Alice Renamed');
    expect(after['id']).toBe(user.id);
    expect(after['username']).toBe(user.username);
    expect(after['email']).toBe(user.email);
    expect(after['role']).toBe(user.role);
    expect(after['provider']).toBe(user.provider);
  });

  it.each([
    ['no full_name', {}],
    ['a null full_name', { full_name: null }],
    ['a full_name that is not a string', { full_name: 42 }],
    ['an empty full_name', { full_name: '' }],
    ['a body that is not an object', 'just a string'],
    ['a body that is an array', ['full_name']],
  ])('refuses an update with %s, changing nothing', async (_name, body) => {
    const { app, key } = await signedIn();

    const response = await app.request(`${MOUNT}/user`, withJson('PATCH', body, key));
    const after = (await (await app.request(`${MOUNT}/user`, as(key))).json()) as Record<
      string,
      unknown
    >;

    expect(response.status).toBe(400);
    expect(after['full_name']).toBe('Alice Example');
  });
});
