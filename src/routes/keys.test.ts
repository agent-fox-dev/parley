import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser } from '../testutil/identity.js';
import { findCredential } from '../store/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const refresh = (keyId: string) => `${MOUNT}/user/keys/${keyId}/refresh`;

describe('refreshing a key the service will not rotate', () => {
  it.each([
    ['a caller holding a personal access token', 'pat', 401],
    ['a key that was revoked', 'revoked', 400],
    ['a key that has expired', 'expired', 400],
    ['a key belonging to another user', 'foreign', 404],
  ] as const)('answers %s with %i and rotates nothing', async (_name, which, expected) => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    const live = await seedApiKey(store, user);
    const pat = await seedPat(store, user, ['keys:manage']);
    const revoked = await seedApiKey(store, user, { revoked_at: '2026-02-01T00:00:00Z' });
    const expired = await seedApiKey(store, user, { expires_at: '2026-02-01T00:00:00Z' });
    const stranger = await seedUser(store);
    const foreign = await seedApiKey(store, stranger);

    const target = { pat: live, revoked, expired, foreign }[which];
    const credential = which === 'pat' ? pat.presented : live.presented;
    const before = await findCredential(store, 'api_key', target.record.id);

    const response = await app.request(
      refresh(target.record.id),
      as(credential, { method: 'POST' }),
    );

    expect(response.status).toBe(expected);
    expect((await findCredential(store, 'api_key', target.record.id))?.secret_hash).toBe(
      before?.secret_hash,
    );
  });
});

describe('a key identifier the caller may not see', () => {
  it('is answered the same whether it belongs to another user or to nobody', async () => {
    service = await identityService();
    const { store, app } = service;
    const user = await seedUser(store);
    const key = await seedApiKey(store, user);
    const stranger = await seedUser(store);
    const foreign = await seedApiKey(store, stranger);

    const answers = await Promise.all(
      [foreign.record.id, 'no-such-key'].flatMap((id) => [
        app.request(`${MOUNT}/user/keys/${id}`, as(key.presented, { method: 'DELETE' })),
        app.request(refresh(id), as(key.presented, { method: 'POST' })),
      ]),
    );
    const seen = await Promise.all(
      answers.map(async (r) => ({ status: r.status, body: await r.json() })),
    );

    expect(seen.map((s) => s.status)).toEqual([404, 404, 404, 404]);
    expect(seen[0]).toEqual(seen[1]);
    expect(seen[2]).toEqual(seen[3]);
  });
});
