import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedPat, seedUser, seedWorkspace } from '../testutil/identity.js';
import { SANDBOX_PATHS } from './sandboxes.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

/** Every sandbox path the reference lists, with the method it answers. */
const PATHS: Array<[string, string]> = [
  ['GET', '/sandbox-profiles'],
  ['POST', '/workspaces/:slug/sandboxes'],
  ['GET', '/workspaces/:slug/sandboxes'],
  ['GET', '/workspaces/:slug/sandboxes/:id'],
  ['DELETE', '/workspaces/:slug/sandboxes/:id'],
  ['GET', '/sandboxes'],
];

const requested = (path: string, slug: string) =>
  `${MOUNT}${path.replace(':slug', slug).replace(':id', 'a-lease')}`;

async function callers() {
  service = await identityService();
  const { store } = service;
  const user = await seedUser(store);
  await seedWorkspace(store, { slug: 'mine', owner_id: user.id });
  await seedWorkspace(store, { slug: 'retired', owner_id: user.id, status: 'archived' });
  return {
    ...service,
    key: (await seedApiKey(store, user)).presented,
    unscoped: (await seedPat(store, user, ['patches:read'])).presented,
  };
}

describe('a sandbox route on a deployment with no provider', () => {
  it.each(PATHS)('answers %s %s with sandboxes_disabled', async (method, path) => {
    const held = await callers();

    const response = await held.app.request(requested(path, 'mine'), as(held.key, { method }));
    const body = (await response.json()) as { error: { message: string; error_type?: string } };

    expect(response.status).toBe(404);
    expect(body.error.error_type).toBe('sandboxes_disabled');
    expect(body.error.message).toBe('sandboxes are not enabled on this hub');
  });

  it.each(PATHS)(
    'answers %s %s the same whatever the workspace and the scope',
    async (method, path) => {
      const held = await callers();

      const answers = await Promise.all([
        held.app.request(requested(path, 'mine'), as(held.key, { method })),
        held.app.request(requested(path, 'retired'), as(held.key, { method })),
        held.app.request(requested(path, 'never-existed'), as(held.key, { method })),
        held.app.request(requested(path, 'mine'), as(held.unscoped, { method })),
      ]);
      const seen = await Promise.all(
        answers.map(async (r) => ({ status: r.status, body: await r.text() })),
      );

      expect(seen.every((s) => s.status === 404)).toBe(true);
      expect(new Set(seen.map((s) => s.body)).size).toBe(1);
    },
  );

  it('registers every path, so none falls through to the unmatched answer', async () => {
    const held = await callers();

    const unmatched = await held.app.request(`${MOUNT}/not-a-route`, as(held.key));
    const body = (await unmatched.json()) as { error: Record<string, unknown> };

    expect(Object.keys(body.error)).not.toContain('error_type');
    expect(SANDBOX_PATHS.length).toBe(PATHS.length);
  });
});

describe('a sandbox route reached with no credential', () => {
  it.each(PATHS)('refuses %s %s for the credential, not the provider', async (method, path) => {
    const held = await callers();

    const response = await held.app.request(requested(path, 'mine'), { method });
    const body = (await response.json()) as { error: { message: string; error_type?: string } };

    expect(response.status).toBe(401);
    expect(body.error.message).toBe('authentication required');
    expect(body.error.error_type).toBeUndefined();
  });
});
