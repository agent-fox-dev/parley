import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import type { WorkspaceStatus } from '../store/workspaces.js';
import { addMember } from '../store/identity.js';
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

async function populated() {
  const held = await owner();
  const { store } = held;
  const stranger = await seedUser(store);
  const theirOrg = await seedOrg(store, stranger);

  const at = async (
    iso: string,
    slug: string,
    ownerId: string,
    orgId: string,
    status: WorkspaceStatus = 'pending',
  ) =>
    seedWorkspace(store, {
      slug,
      owner_id: ownerId,
      org_id: orgId,
      status,
      created_at: iso,
      updated_at: iso,
    });

  // created_at descending, and slug ascending where two share an instant
  await at('2026-01-03T00:00:00.000Z', 'newest', held.user.id, held.org.id);
  await at('2026-01-01T00:00:00.000Z', 'beta', held.user.id, held.org.id);
  await at('2026-01-01T00:00:00.000Z', 'alpha', held.user.id, held.org.id);
  await at('2026-01-02T00:00:00.000Z', 'retired', held.user.id, held.org.id, 'archived');
  await at('2026-01-04T00:00:00.000Z', 'someone-elses', stranger.id, theirOrg.id);

  return held;
}

describe('the workspace list', () => {
  it('shows the caller their own and omits other people’s', async () => {
    const held = await populated();

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.key))
    ).json()) as Array<{ slug: string }>;

    expect(listed.map((w) => w.slug)).not.toContain('someone-elses');
  });

  it('orders newest first, and by slug where two share an instant', async () => {
    const held = await populated();

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.key))
    ).json()) as Array<{ slug: string }>;

    expect(listed.map((w) => w.slug)).toEqual(['newest', 'alpha', 'beta']);
  });

  it('leaves archived workspaces out until they are asked for', async () => {
    const held = await populated();

    const without = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.key))
    ).json()) as Array<{ slug: string }>;
    const withArchived = (await (
      await held.app.request(`${MOUNT}/workspaces?include_archived=true`, as(held.key))
    ).json()) as Array<{ slug: string }>;

    expect(without.map((w) => w.slug)).not.toContain('retired');
    expect(withArchived.map((w) => w.slug)).toEqual(['newest', 'retired', 'alpha', 'beta']);
  });

  it('shows an admin token every workspace, as admin', async () => {
    const held = await populated();

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(ADMIN_TOKEN))
    ).json()) as Array<{ slug: string; access: string }>;

    expect(listed.map((w) => w.slug)).toContain('someone-elses');
    expect(listed.every((w) => w.access === 'admin')).toBe(true);
  });

  it('shows a member their organization’s workspaces as member', async () => {
    const held = await populated();
    const member = await seedUser(held.store);
    await addMember(held.store, {
      org_id: held.org.id,
      user_id: member.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    const memberKey = (await seedApiKey(held.store, member)).presented;

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(memberKey))
    ).json()) as Array<{ slug: string; access: string }>;

    expect(listed.map((w) => w.slug)).toEqual(['newest', 'alpha', 'beta']);
    expect(listed.every((w) => w.access === 'member')).toBe(true);
  });

  it('gives a member the owner’s object, differing only in access', async () => {
    const held = await owner();
    const member = await seedUser(held.store);
    await addMember(held.store, {
      org_id: held.org.id,
      user_id: member.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    const memberKey = (await seedApiKey(held.store, member)).presented;
    await held.app.request(`${MOUNT}/workspaces`, withJson('POST', create(), held.key));

    const byOwner = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.key))
    ).json()) as Array<Record<string, unknown>>;
    const byMember = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(memberKey))
    ).json()) as Array<Record<string, unknown>>;

    expect({ ...byMember[0], access: null }).toEqual({ ...byOwner[0], access: null });
    expect(byMember[0]?.['access']).toBe('member');
  });
});

async function updatable() {
  const held = await owner();
  await held.app.request(`${MOUNT}/workspaces`, withJson('POST', create(), held.key));
  return held;
}

const read = async (held: Awaited<ReturnType<typeof owner>>) =>
  (await (await held.app.request(`${MOUNT}/workspaces/my-project`, as(held.key))).json()) as Record<
    string,
    unknown
  >;

const patch = async (held: Awaited<ReturnType<typeof owner>>, body: unknown) =>
  held.app.request(`${MOUNT}/workspaces/my-project`, withJson('PATCH', body, held.key));

describe('updating a workspace', () => {
  it('changes the field it names and leaves every other alone', async () => {
    const held = await updatable();
    const before = await read(held);

    const response = await patch(held, { display_name: 'Renamed' });
    const after = await read(held);

    expect(response.status).toBe(200);
    expect(after['display_name']).toBe('Renamed');
    expect({ ...after, display_name: null, updated_at: null }).toEqual({
      ...before,
      display_name: null,
      updated_at: null,
    });
  });

  it('returns a field given as null to its default', async () => {
    const held = await updatable();

    await patch(held, { display_name: 'Renamed', description: 'a description' });
    await patch(held, { description: null });
    const withoutDescription = await read(held);
    await patch(held, { display_name: null });
    const withoutName = await read(held);

    expect(withoutDescription['description']).toBe('');
    expect(withoutDescription['display_name']).toBe('Renamed');
    expect(withoutName['display_name']).toBe('my-project');
  });

  it('returns every setting to its default when the settings object is null', async () => {
    const held = await updatable();
    await patch(held, { settings: { rebuild_strategy: 'merge', sync_enabled: false } });

    const response = await patch(held, { settings: null });
    const settings = ((await response.json()) as { settings: Record<string, unknown> }).settings;

    expect(response.status).toBe(200);
    expect(settings['rebuild_strategy']).toBe('rebase');
    expect(settings['sync_enabled']).toBe(true);
  });

  it('sets, resets and keeps settings in one call', async () => {
    const held = await updatable();
    await patch(held, { settings: { sync_enabled: false, patch_branch_source: 'origin' } });

    const response = await patch(held, {
      settings: { rebuild_strategy: 'merge', sync_enabled: null },
    });
    const settings = ((await response.json()) as { settings: Record<string, unknown> }).settings;

    expect(settings['rebuild_strategy']).toBe('merge');
    expect(settings['sync_enabled']).toBe(true);
    expect(settings['patch_branch_source']).toBe('origin');
  });

  it('moves the workspace to another organization its owner belongs to', async () => {
    const held = await updatable();
    const second = await seedOrg(held.store, held.user);

    const response = await patch(held, { org_id: second.slug });

    expect(response.status).toBe(200);
    expect(((await response.json()) as { org_id: string }).org_id).toBe(second.id);
  });
});

describe('an update the service refuses', () => {
  it.each([
    ['an empty body', {}],
    ['a 129-character display_name', { display_name: 'x'.repeat(129) }],
    ['a 1025-character description', { description: 'y'.repeat(1025) }],
    ['the immutable slug', { slug: 'renamed-project' }],
    ['the immutable git_url', { git_url: 'https://github.com/other/repo.git' }],
    ['the immutable branch', { branch: 'trunk' }],
    ['the immutable owner_id', { owner_id: 'somebody-else' }],
    ['the immutable workspace_mode', { workspace_mode: 'carry_patch' }],
    ['the immutable upstream_url', { upstream_url: 'https://up.test/r.git' }],
    ['the immutable integration_branch', { integration_branch: 'deploy' }],
    ['the retired sync_mode', { sync_mode: 'manual' }],
    ['an org_id naming nothing', { org_id: 'no-such-org' }],
    ['an unknown setting', { settings: { invented_switch: true } }],
    ['a setting value outside its list', { settings: { rebuild_strategy: 'squash' } }],
  ])('answers 400 for %s and changes nothing', async (_name, body) => {
    const held = await updatable();
    const before = await read(held);

    const response = await patch(held, body);

    expect(response.status).toBe(400);
    expect(await read(held)).toEqual(before);
  });

  it('answers 400 on an archived workspace, after the access check has passed', async () => {
    const held = await updatable();
    await held.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
      'archived',
      'my-project',
    ]);

    const response = await patch(held, { display_name: 'Renamed' });

    expect(response.status).toBe(400);
  });

  it("answers 403 for an organization the workspace's owner does not belong to", async () => {
    const held = await updatable();
    const stranger = await seedUser(held.store);
    const theirs = await seedOrg(held.store, stranger);
    const before = await read(held);

    const response = await patch(held, { org_id: theirs.id });

    expect(response.status).toBe(403);
    expect(await read(held)).toEqual(before);
  });
});
