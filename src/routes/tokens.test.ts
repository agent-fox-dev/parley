import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const tokens = `${MOUNT}/user/tokens`;

async function holder(permissions: readonly string[] = ['tokens:manage', 'tokens:read']) {
  service = await identityService();
  const user = await seedUser(service.store);
  return {
    ...service,
    user,
    key: (await seedApiKey(service.store, user)).presented,
    pat: (await seedPat(service.store, user, permissions)).presented,
  };
}

describe('listing the caller’s tokens', () => {
  it('shows their permissions and never a secret', async () => {
    const { app, store, user, key } = await holder();
    const first = await seedPat(store, user, ['workspaces:read']);
    const second = await seedPat(store, user, ['patches:read', 'patches:write'], {
      name: 'the other one',
    });

    const response = await app.request(tokens, as(key));
    const listed = (await response.json()) as Array<Record<string, unknown>>;
    const byId = new Map(listed.map((entry) => [entry['token_id'], entry]));

    expect(response.status).toBe(200);
    expect(byId.get(first.record.id)?.['permissions']).toEqual(['workspaces:read']);
    expect(byId.get(second.record.id)?.['permissions']).toEqual(['patches:read', 'patches:write']);
    for (const entry of listed) {
      expect(Object.keys(entry).toSorted()).toEqual([
        'created_at',
        'expires_at',
        'name',
        'permissions',
        'revoked_at',
        'token_id',
      ]);
    }
    expect(JSON.stringify(listed)).not.toContain(first.record.secret_hash);
  });
});

describe('what a caller may grant', () => {
  it.each([
    ['a token granting scopes it holds itself', 'pat', ['workspaces:read'], 201],
    ['a token granting a scope it does not hold', 'pat', ['workspaces:write'], 403],
    ['an API key granting any registered scope', 'key', ['workspaces:write'], 201],
  ] as const)('answers %s with %i', async (_name, who, permissions, expected) => {
    const held = await holder(['tokens:manage', 'workspaces:read']);
    const before = await held.store.all('SELECT id FROM credentials');

    const response = await held.app.request(
      tokens,
      withJson('POST', { name: 'a token', permissions }, held[who]),
    );
    const after = await held.store.all('SELECT id FROM credentials');

    expect(response.status).toBe(expected);
    expect(after.length).toBe(before.length + (expected === 201 ? 1 : 0));
  });
});

describe('a token request the service refuses', () => {
  it.each([
    ['no name', { permissions: ['workspaces:read'] }],
    ['a name that is not a string', { name: 7, permissions: ['workspaces:read'] }],
    [
      'a name longer than 255 characters',
      { name: 'x'.repeat(256), permissions: ['workspaces:read'] },
    ],
    ['no permissions', { name: 'a token' }],
    ['an empty permission list', { name: 'a token', permissions: [] }],
    ['a permission list that is not an array', { name: 'a token', permissions: 'workspaces:read' }],
    ['a retired scope name', { name: 'a token', permissions: ['workspaces:create'] }],
    ['an unregistered scope name', { name: 'a token', permissions: ['invented:scope'] }],
    [
      'a lifetime that is not allowed',
      { name: 'a token', permissions: ['workspaces:read'], expires: 45 },
    ],
    ['a body that is not an object', 'a string'],
  ])('answers 400 for %s and writes no token', async (_name, body) => {
    const { app, store, key } = await holder();
    const before = await store.all('SELECT id FROM credentials');

    const response = await app.request(tokens, withJson('POST', body, key));

    expect(response.status).toBe(400);
    expect(await store.all('SELECT id FROM credentials')).toEqual(before);
  });

  it('answers 400 for a token that is already revoked, leaving it as it was', async () => {
    const { app, store, user, key } = await holder();
    const revoked = await seedPat(store, user, ['workspaces:read'], {
      revoked_at: '2026-02-01T00:00:00Z',
    });

    const response = await app.request(
      `${tokens}/${revoked.record.id}`,
      as(key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(400);
    expect(
      (await store.all('SELECT revoked_at FROM credentials WHERE id = ?', [revoked.record.id]))[0],
    ).toEqual({ revoked_at: '2026-02-01T00:00:00Z' });
  });

  it.each(['GET', 'DELETE'])('answers 404 to %s for a token of another user', async (method) => {
    const { app, store, key } = await holder();
    const stranger = await seedUser(store);
    const foreign = await seedPat(store, stranger, ['workspaces:read']);

    const response = await app.request(`${tokens}/${foreign.record.id}`, as(key, { method }));

    expect(response.status).toBe(404);
  });
});
