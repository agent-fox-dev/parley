import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedUser } from '../testutil/identity.js';

const KEY_FIELDS = ['created_at', 'expires_at', 'key_id', 'revoked_at'];

let service: Service;

afterEach(() => {
  service.dispose();
});

describe("the caller's keys", () => {
  it('are listed as metadata alone, and a matching ETag answers 304', async () => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    const live = await seedApiKey(store, user);
    await seedApiKey(store, user, { revoked_at: '2026-02-01T00:00:00Z' });
    await seedApiKey(store, user, { expires_at: '2026-02-01T00:00:00Z' });

    const listed = await app.request(`${MOUNT}/user/keys`, as(live.presented));
    const entries = (await listed.json()) as Array<Record<string, unknown>>;
    const etag = listed.headers.get('etag');

    expect(listed.status).toBe(200);
    expect(entries).toHaveLength(3);
    for (const entry of entries) {
      expect(Object.keys(entry).toSorted()).toEqual(KEY_FIELDS);
    }
    expect(JSON.stringify(entries)).not.toContain(live.record.secret_hash);
    expect(etag).not.toBeNull();

    const repeated = await app.request(
      `${MOUNT}/user/keys`,
      as(live.presented, { headers: { 'if-none-match': etag ?? '' } }),
    );

    expect(repeated.status).toBe(304);
    expect(await repeated.text()).toBe('');
  });

  it('are listed without another user’s', async () => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    const key = await seedApiKey(store, user);
    const stranger = await seedUser(store);
    await seedApiKey(store, stranger);

    const entries = (await (
      await app.request(`${MOUNT}/user/keys`, as(key.presented))
    ).json()) as Array<{ key_id: string }>;

    expect(entries.map((entry) => entry.key_id)).toEqual([key.record.id]);
  });
});

describe('rotating a key', () => {
  it('keeps the identifier, recomputes the expiry and retires the old secret', async () => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    // Issued five days ago for ten, so a renewal has somewhere to move to.
    const day = 24 * 60 * 60 * 1000;
    const key = await seedApiKey(store, user, {
      created_at: new Date(SEED_NOW.getTime() - 5 * day).toISOString(),
      expires_at: new Date(SEED_NOW.getTime() + 5 * day).toISOString(),
    });

    const response = await app.request(
      `${MOUNT}/user/keys/${key.record.id}/refresh`,
      as(key.presented, { method: 'POST' }),
    );
    const body = (await response.json()) as {
      key: string;
      key_id: string;
      expires_at: string | null;
    };

    expect(response.status).toBe(200);
    expect(body.key_id).toBe(key.record.id);
    expect(body.key).not.toBe(key.presented);
    expect(body.key.startsWith(`af_${key.record.id}_`)).toBe(true);
    expect(body.expires_at).toBe(new Date(SEED_NOW.getTime() + 10 * day).toISOString());

    expect((await app.request(`${MOUNT}/user`, as(key.presented))).status).toBe(401);
    expect((await app.request(`${MOUNT}/user`, as(body.key))).status).toBe(200);
  });
});

describe('revoking a key', () => {
  it('works for another key of the caller and for the one making the request', async () => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    const first = await seedApiKey(store, user);
    const second = await seedApiKey(store, user);

    const other = await app.request(
      `${MOUNT}/user/keys/${first.record.id}`,
      as(second.presented, { method: 'DELETE' }),
    );
    const itself = await app.request(
      `${MOUNT}/user/keys/${second.record.id}`,
      as(second.presented, { method: 'DELETE' }),
    );

    expect(other.status).toBe(204);
    expect(itself.status).toBe(204);
    expect((await app.request(`${MOUNT}/user`, as(first.presented))).status).toBe(401);
    expect((await app.request(`${MOUNT}/user`, as(second.presented))).status).toBe(401);
  });
});
