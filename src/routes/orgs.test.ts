import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

describe('reading an organization the caller has no claim on', () => {
  it('answers 403 when it exists and the caller is not a member', async () => {
    service = await identityService();
    const { app, store } = service;
    const stranger = await seedUser(store);
    const owner = await seedUser(store);
    const org = await seedOrg(store, owner);
    const key = await seedApiKey(store, stranger);

    const response = await app.request(`${MOUNT}/orgs/${org.id}`, as(key.presented));

    expect(response.status).toBe(403);
  });

  it('answers 404 when no organization has that id', async () => {
    service = await identityService();
    const { app, store } = service;
    const user = await seedUser(store);
    const key = await seedApiKey(store, user);

    const response = await app.request(`${MOUNT}/orgs/no-such-org`, as(key.presented));

    expect(response.status).toBe(404);
  });
});
