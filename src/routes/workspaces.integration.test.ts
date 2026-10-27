import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';
import { WORKSPACE_SETTINGS } from '../workspace/settings.js';
import { workspaceScope } from '../platform/secrets.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const create = (over: Record<string, unknown> = {}) => ({
  slug: 'my-project',
  git_url: 'https://github.com/user/repo.git',
  ...over,
});

async function owner() {
  service = await identityService();
  const user = await seedUser(service.store);
  const org = await seedOrg(service.store, user);
  return { ...service, user, org, key: (await seedApiKey(service.store, user)).presented };
}

describe('creating a workspace', () => {
  it('writes a pending workspace that reads back with every setting at its effective value', async () => {
    const held = await owner();

    const created = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson(
        'POST',
        create({ settings: { rebuild_strategy: 'merge', sync_enabled: false } }),
        held.key,
      ),
    );
    const workspace = (await created.json()) as Record<string, unknown>;

    expect(created.status).toBe(201);
    expect(workspace['status']).toBe('pending');
    expect(workspace['access']).toBe('owner');
    expect(workspace['display_name']).toBe('my-project');
    expect(workspace['description']).toBe('');
    expect(workspace['owner_id']).toBe(held.user.id);
    expect(workspace['org_id']).toBe(held.org.id);

    const settings = workspace['settings'] as Record<string, unknown>;
    expect(Object.keys(settings).toSorted()).toEqual(Object.keys(WORKSPACE_SETTINGS).toSorted());
    expect(settings['rebuild_strategy']).toBe('merge');
    expect(settings['sync_enabled']).toBe(false);
    expect(settings['patch_branch_source']).toBe('hub');
    expect(settings['auto_rebuild_after_sync']).toBe(true);

    const read = await held.app.request(`${MOUNT}/workspaces/my-project`, as(held.key));
    expect(read.status).toBe(200);
    expect(await read.json()).toEqual(workspace);
  });
});

describe('the organization a workspace is created in', () => {
  it("is the caller's own when the body names none", async () => {
    const held = await owner();

    const created = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create(), held.key),
    );

    expect(((await created.json()) as { org_id: string }).org_id).toBe(held.org.id);
  });

  it('is the one the body names, by id or by slug', async () => {
    const held = await owner();
    const second = await seedOrg(held.store, held.user);

    const byId = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create({ slug: 'by-id', org_id: second.id }), held.key),
    );
    const bySlug = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create({ slug: 'by-slug', org_id: second.slug }), held.key),
    );

    expect(((await byId.json()) as { org_id: string }).org_id).toBe(second.id);
    expect(((await bySlug.json()) as { org_id: string }).org_id).toBe(second.id);
  });

  it('is refused 400 when the body names no organization that exists', async () => {
    const held = await owner();

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create({ org_id: 'no-such-org' }), held.key),
    );

    expect(response.status).toBe(400);
    expect(await held.store.all('SELECT slug FROM workspaces')).toEqual([]);
  });

  it('is refused 403 when the caller is not a member of the one it names', async () => {
    const held = await owner();
    const stranger = await seedUser(held.store);
    const theirs = await seedOrg(held.store, stranger);

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create({ org_id: theirs.id }), held.key),
    );

    expect(response.status).toBe(403);
    expect(await held.store.all('SELECT slug FROM workspaces')).toEqual([]);
  });

  it('is refused 400 when the caller has no organization of their own', async () => {
    service = await identityService();
    const loner = await seedUser(service.store);
    const key = (await seedApiKey(service.store, loner)).presented;

    const response = await service.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create(), key),
    );

    expect(response.status).toBe(400);
  });
});

describe('the credentials a create carries', () => {
  it.each([
    ['a token', { git_pat: 'ghp_SENTINEL9f3a' }, ['GIT_PAT'], 'ghp_SENTINEL9f3a'],
    [
      'a basic pair',
      { git_username: 'user', git_password: 'pw_SENTINEL7c21' },
      ['GIT_PASSWORD', 'GIT_USERNAME'],
      'pw_SENTINEL7c21',
    ],
  ])('are stored as sealed workspace secrets: %s', async (_name, given, names, sentinel) => {
    const held = await owner();

    const created = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', create({ ...given }), held.key),
    );
    const text = await created.text();

    expect(created.status).toBe(201);
    expect(text).not.toContain(sentinel);
    expect((await held.secrets.names(workspaceScope('my-project'))).toSorted()).toEqual(names);

    const raw = await held.store.all('SELECT * FROM workspace_secrets');
    expect(JSON.stringify(raw)).not.toContain(sentinel);
    expect(await held.secrets.get(workspaceScope('my-project'), names[0] as string)).toBeDefined();
  });

  it('leaves no secret behind for a create that carried none', async () => {
    const held = await owner();

    await held.app.request(`${MOUNT}/workspaces`, withJson('POST', create(), held.key));

    expect(await held.secrets.names(workspaceScope('my-project'))).toEqual([]);
  });
});
