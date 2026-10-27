import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

describe('replacing a token’s permissions with an empty list', () => {
  it('removes every permission and revokes the token', async () => {
    service = await identityService([
      {
        path: `${MOUNT}/needs-patches-read`,
        scope: 'patches:read',
        handler: () => new Response('{}', { status: 200 }),
      },
    ]);
    const { app, store } = service;
    const user = await seedUser(store);
    const key = await seedApiKey(store, user);
    const token = await seedPat(store, user, ['patches:read', 'workspaces:read']);

    expect((await app.request(`${MOUNT}/needs-patches-read`, as(token.presented))).status).toBe(
      200,
    );

    const response = await app.request(
      `${MOUNT}/user/tokens/${token.record.id}/permissions`,
      withJson('PUT', { permissions: [] }, key.presented),
    );
    const body = (await response.json()) as { permissions: string[]; revoked_at: string | null };

    expect(response.status).toBe(200);
    expect(body.permissions).toEqual([]);
    expect(body.revoked_at).not.toBeNull();
    expect((await app.request(`${MOUNT}/needs-patches-read`, as(token.presented))).status).toBe(
      401,
    );
  });
});
