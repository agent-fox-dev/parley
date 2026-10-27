import { afterEach, describe, expect, it } from 'vitest';
import { type Service, ADMIN_TOKEN, MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const body = (over: Record<string, unknown> = {}) => ({
  slug: 'my-project',
  git_url: 'https://github.com/user/repo.git',
  ...over,
});

async function callers() {
  service = await identityService();
  const { store } = service;
  const user = await seedUser(store);
  await seedOrg(store, user);
  return {
    ...service,
    user,
    key: (await seedApiKey(store, user)).presented,
    scoped: (await seedPat(store, user, ['workspaces:write'])).presented,
    unscoped: (await seedPat(store, user, ['patches:read'])).presented,
  };
}

describe('who may create a workspace', () => {
  it('refuses an admin token, because a workspace needs a real user as its owner', async () => {
    const held = await callers();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', body(), ADMIN_TOKEN),
    );

    expect(response.status).toBe(403);
    expect(await held.store.all('SELECT slug FROM workspaces')).toEqual([]);
  });

  it('refuses a token without workspaces:write with 403, not the anti-enumeration 404', async () => {
    const held = await callers();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', body(), held.unscoped),
    );

    expect(response.status).toBe(403);
  });

  it('admits a token holding workspaces:write', async () => {
    const held = await callers();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', body({ slug: 'scoped-create' }), held.scoped),
    );

    expect(response.status).toBe(201);
  });

  it('admits an API key', async () => {
    const held = await callers();

    expect(
      (await held.app.request(`${MOUNT}/workspaces`, withJson('POST', body(), held.key))).status,
    ).toBe(201);
  });
});

describe('a create the service refuses', () => {
  it('answers 409 for a slug that already names a workspace, writing nothing more', async () => {
    const held = await callers();

    expect(
      (await held.app.request(`${MOUNT}/workspaces`, withJson('POST', body(), held.key))).status,
    ).toBe(201);
    const again = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', body({ git_url: 'https://github.com/other/repo.git' }), held.key),
    );

    expect(again.status).toBe(409);
    expect(await held.store.all('SELECT slug FROM workspaces')).toHaveLength(1);
  });

  it.each([
    ['a malformed slug', body({ slug: 'Not-A-Slug' })],
    ['no git_url', { slug: 'a-project' }],
    ['an unknown setting', body({ settings: { invented_switch: true } })],
    ['a setting value the setting cannot take', body({ settings: { rebuild_strategy: 'squash' } })],
    ['a credential with an ssh url', body({ git_url: 'git@github.com:u/r.git', git_pat: 'p' })],
  ])('answers 400 for %s and writes neither a workspace nor a secret', async (_name, given) => {
    const held = await callers();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', given, held.key),
    );

    expect(response.status).toBe(400);
    expect(await held.store.all('SELECT slug FROM workspaces')).toEqual([]);
    expect(await held.store.all('SELECT key FROM workspace_secrets')).toEqual([]);
  });

  it('names the setting and its choices when a value is refused', async () => {
    const held = await callers();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', body({ settings: { rebuild_strategy: 'squash' } }), held.key),
    );
    const message = ((await response.json()) as { error: { message: string } }).error.message;

    expect(message).toContain('rebuild_strategy');
    expect(message).toContain('rebase');
    expect(message).toContain('merge');
  });
});
