import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedUser } from '../testutil/identity.js';

const tokens = `${MOUNT}/user/tokens`;

let service: Service;

afterEach(() => {
  service.dispose();
});

async function signedIn() {
  service = await identityService([
    {
      path: `${MOUNT}/needs-patches-read`,
      scope: 'patches:read',
      handler: () => new Response('{}', { status: 200 }),
    },
  ]);
  const user = await seedUser(service.store);
  return { ...service, user, key: (await seedApiKey(service.store, user)).presented };
}

interface CreatedToken {
  token_id: string;
  name: string;
  token: string;
  permissions: string[];
  expires_at: string | null;
  created_at: string;
}

describe('creating a token', () => {
  it('returns it once, and it then authenticates on its own scope', async () => {
    const { app, key } = await signedIn();

    const response = await app.request(
      tokens,
      withJson('POST', { name: 'CI token', permissions: ['patches:read'], expires: 30 }, key),
    );
    const created = (await response.json()) as CreatedToken;

    expect(response.status).toBe(201);
    expect(created.token).toMatch(/^af_pat_[0-9a-f]+_[0-9a-f]+$/);
    expect(created.token.startsWith(`af_pat_${created.token_id}_`)).toBe(true);
    expect(created.permissions).toEqual(['patches:read']);
    expect(created.expires_at).toBe(
      new Date(SEED_NOW.getTime() + 30 * 24 * 60 * 60 * 1000).toISOString(),
    );

    expect((await app.request(`${MOUNT}/needs-patches-read`, as(created.token))).status).toBe(200);

    const listed = await (await app.request(tokens, as(key))).text();
    const read = await (await app.request(`${tokens}/${created.token_id}`, as(key))).text();

    expect(listed).not.toContain(created.token);
    expect(read).not.toContain(created.token);
  });
});

describe('revoking a token', () => {
  it('answers with its metadata, not an empty body, and stops it working', async () => {
    const { app, key } = await signedIn();
    const created = (await (
      await app.request(
        tokens,
        withJson('POST', { name: 'CI token', permissions: ['patches:read'] }, key),
      )
    ).json()) as CreatedToken;

    expect((await app.request(`${MOUNT}/needs-patches-read`, as(created.token))).status).toBe(200);

    const response = await app.request(
      `${tokens}/${created.token_id}`,
      as(key, { method: 'DELETE' }),
    );
    const body = (await response.json()) as { token_id: string; revoked_at: string | null };

    expect(response.status).toBe(200);
    expect(body.token_id).toBe(created.token_id);
    expect(body.revoked_at).not.toBeNull();
    expect((await app.request(`${MOUNT}/needs-patches-read`, as(created.token))).status).toBe(401);
  });
});
