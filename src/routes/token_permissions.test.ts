import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function tokenHolding(permissions: readonly string[]) {
  service = await identityService();
  const user = await seedUser(service.store);
  const key = await seedApiKey(service.store, user);
  const token = await seedPat(service.store, user, permissions);
  return {
    ...service,
    key: key.presented,
    token,
    path: `${MOUNT}/user/tokens/${token.record.id}/permissions`,
  };
}

async function permissionsOf(from: Service, id: string): Promise<string[]> {
  const rows = await from.store.all<{ permissions: string }>(
    'SELECT permissions FROM credentials WHERE id = ?',
    [id],
  );
  const stored = rows[0]?.permissions ?? '';
  return stored.length === 0 ? [] : stored.split(' ');
}

describe('changing a token’s permissions', () => {
  it('replaces the set on PUT, adds on PATCH and removes on DELETE', async () => {
    const held = await tokenHolding(['workspaces:read', 'patches:read']);

    const replaced = await held.app.request(
      held.path,
      withJson('PUT', { permissions: ['merges:read'] }, held.key),
    );
    expect(replaced.status).toBe(200);
    expect(await permissionsOf(held, held.token.record.id)).toEqual(['merges:read']);

    const added = await held.app.request(
      held.path,
      withJson('PATCH', { permissions: ['patches:write'] }, held.key),
    );
    expect(added.status).toBe(200);
    expect(await permissionsOf(held, held.token.record.id)).toEqual([
      'merges:read',
      'patches:write',
    ]);

    const removed = await held.app.request(
      held.path,
      withJson('DELETE', { permissions: ['merges:read'] }, held.key),
    );
    const body = (await removed.json()) as { permissions: string[]; token_id: string };
    expect(removed.status).toBe(200);
    expect(body.permissions).toEqual(['patches:write']);
    expect(body.token_id).toBe(held.token.record.id);
  });

  it('treats removing a scope the token does not hold as a success that changes nothing', async () => {
    const held = await tokenHolding(['workspaces:read']);

    const response = await held.app.request(
      held.path,
      withJson('DELETE', { permissions: ['sandboxes:write'] }, held.key),
    );

    expect(response.status).toBe(200);
    expect(await permissionsOf(held, held.token.record.id)).toEqual(['workspaces:read']);
  });
});

describe('a permission change the service refuses', () => {
  it.each([
    ['PATCH with an empty array', 'PATCH', { permissions: [] }],
    ['DELETE with an empty array', 'DELETE', { permissions: [] }],
    ['PUT with no permissions key', 'PUT', {}],
    ['PATCH with no permissions key', 'PATCH', {}],
    ['DELETE with no permissions key', 'DELETE', {}],
    ['PUT with a null list', 'PUT', { permissions: null }],
    ['PATCH with a null list', 'PATCH', { permissions: null }],
    ['DELETE with a null list', 'DELETE', { permissions: null }],
    ['PUT naming a retired scope', 'PUT', { permissions: ['vars:manage'] }],
  ])('answers 400 to %s, changing nothing', async (_name, method, body) => {
    const held = await tokenHolding(['workspaces:read', 'patches:read']);

    const response = await held.app.request(held.path, withJson(method, body, held.key));

    expect(response.status).toBe(400);
    expect(await permissionsOf(held, held.token.record.id)).toEqual([
      'workspaces:read',
      'patches:read',
    ]);
    expect(
      (
        await held.store.all('SELECT revoked_at FROM credentials WHERE id = ?', [
          held.token.record.id,
        ])
      )[0],
    ).toEqual({ revoked_at: null });
  });

  it.each(['PUT', 'PATCH', 'DELETE'])('answers 400 to %s on a revoked token', async (method) => {
    service = await identityService();
    const user = await seedUser(service.store);
    const key = await seedApiKey(service.store, user);
    const revoked = await seedPat(service.store, user, ['workspaces:read'], {
      revoked_at: '2026-02-01T00:00:00Z',
    });

    const response = await service.app.request(
      `${MOUNT}/user/tokens/${revoked.record.id}/permissions`,
      withJson(method, { permissions: ['patches:read'] }, key.presented),
    );

    expect(response.status).toBe(400);
    expect(await permissionsOf(service, revoked.record.id)).toEqual(['workspaces:read']);
  });

  it('answers 403 when a token adds a scope it does not hold itself', async () => {
    const held = await tokenHolding(['workspaces:read', 'tokens:write']);

    const response = await held.app.request(
      held.path,
      withJson('PATCH', { permissions: ['merges:write'] }, held.token.presented),
    );

    expect(response.status).toBe(403);
    expect(await permissionsOf(held, held.token.record.id)).toEqual([
      'workspaces:read',
      'tokens:write',
    ]);
  });
});
