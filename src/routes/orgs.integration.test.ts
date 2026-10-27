import { afterEach, describe, expect, it } from 'vitest';
import { type Service, ADMIN_TOKEN, MOUNT, as, identityService } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';
import { addMember, isMember } from '../store/identity.js';

const ORG_FIELDS = ['created_at', 'id', 'name', 'owner_id', 'slug', 'status', 'updated_at', 'url'];

let service: Service;

afterEach(() => {
  service.dispose();
});

describe("a caller's organizations", () => {
  it('list the personal one and every other they belong to', async () => {
    service = await identityService();
    const { app, store } = service;
    const user = await seedUser(store);
    const personal = await seedOrg(store, user);
    const other = await seedOrg(store, await seedUser(store));
    await addMember(store, {
      org_id: other.id,
      user_id: user.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    const key = await seedApiKey(store, user);

    const response = await app.request(`${MOUNT}/user/orgs`, as(key.presented));
    const listed = (await response.json()) as Array<Record<string, unknown>>;

    expect(response.status).toBe(200);
    expect(listed.map((org) => org['slug']).toSorted()).toEqual(
      [personal.slug, other.slug].toSorted(),
    );
    for (const org of listed) {
      expect(Object.keys(org).toSorted()).toEqual(ORG_FIELDS);
    }
  });

  it('omit one the caller does not belong to', async () => {
    service = await identityService();
    const { app, store } = service;
    const user = await seedUser(store);
    await seedOrg(store, user);
    const elsewhere = await seedOrg(store, await seedUser(store));
    const key = await seedApiKey(store, user);

    const listed = (await (
      await app.request(`${MOUNT}/user/orgs`, as(key.presented))
    ).json()) as Array<{ id: string }>;

    expect(listed.map((org) => org.id)).not.toContain(elsewhere.id);
  });
});

describe('reading one organization by id', () => {
  it('answers a member and an admin alike, and 304 on a matching ETag', async () => {
    service = await identityService();
    const { app, store } = service;
    const owner = await seedUser(store);
    const org = await seedOrg(store, owner);
    const key = await seedApiKey(store, owner);

    const asMember = await app.request(`${MOUNT}/orgs/${org.id}`, as(key.presented));
    const asAdmin = await app.request(`${MOUNT}/orgs/${org.id}`, as(ADMIN_TOKEN));
    const etag = asMember.headers.get('etag');

    expect(asMember.status).toBe(200);
    expect(asAdmin.status).toBe(200);
    expect(await asAdmin.json()).toEqual(await asMember.json());
    expect(etag).not.toBeNull();

    const repeated = await app.request(
      `${MOUNT}/orgs/${org.id}`,
      as(key.presented, { headers: { 'if-none-match': etag ?? '' } }),
    );

    expect(repeated.status).toBe(304);
    expect(await repeated.text()).toBe('');
  });
});

describe('the membership lookup the later packages read', () => {
  it('answers for a member and for a stranger', async () => {
    service = await identityService();
    const { store } = service;
    const owner = await seedUser(store);
    const org = await seedOrg(store, owner);
    const stranger = await seedUser(store);

    expect(await isMember(store, org.id, owner.id)).toBe(true);
    expect(await isMember(store, org.id, stranger.id)).toBe(false);
  });
});
