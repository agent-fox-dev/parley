import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';
import { WORKSPACE_SETTINGS } from '../workspace/settings.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  const member = await seedUser(store);
  await addMember(store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });

  return {
    ...service,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    paths: {
      user: `${MOUNT}/user/vars`,
      org: `${MOUNT}/orgs/${org.slug}/vars`,
      workspace: `${MOUNT}/workspaces/mine/vars`,
    },
  };
}

describe('a variable at each scope', () => {
  it.each(['user', 'org', 'workspace'] as const)(
    'is created, listed, replaced and deleted with its value at the %s scope',
    async (family) => {
      const held = await world();
      const path = held.paths[family];

      const created = await held.app.request(
        path,
        withJson(
          'POST',
          {
            entries: [
              { key: 'FIRST_VAR', value: 'first value' },
              { key: 'SECOND_VAR', value: 'second value' },
            ],
          },
          held.ownerKey,
        ),
      );
      const createdBody = (await created.json()) as Array<Record<string, unknown>>;

      expect(created.status).toBe(201);
      for (const entry of createdBody) {
        expect(Object.keys(entry).toSorted()).toEqual(['created_at', 'key', 'updated_at', 'value']);
      }
      expect(createdBody.map((e) => e['value'])).toEqual(['first value', 'second value']);

      const listed = await held.app.request(path, as(held.ownerKey));
      const listedBody = (await listed.json()) as Array<{ key: string; value: string }>;
      expect(listed.status).toBe(200);
      expect(listedBody.map((e) => [e.key, e.value])).toEqual([
        ['FIRST_VAR', 'first value'],
        ['SECOND_VAR', 'second value'],
      ]);

      const replaced = await held.app.request(
        `${path}/FIRST_VAR`,
        withJson('PATCH', { value: 'a new value' }, held.ownerKey),
      );
      expect(replaced.status).toBe(200);
      expect(((await replaced.json()) as { value: string }).value).toBe('a new value');

      const removed = await held.app.request(
        `${path}/SECOND_VAR`,
        as(held.ownerKey, { method: 'DELETE' }),
      );
      expect(removed.status).toBe(204);

      const after = (await (await held.app.request(path, as(held.ownerKey))).json()) as Array<{
        key: string;
        value: string;
      }>;
      expect(after).toEqual([expect.objectContaining({ key: 'FIRST_VAR', value: 'a new value' })]);
    },
  );
});

describe("a member reading a workspace's variables", () => {
  it('is answered exactly what the owner is answered, values included', async () => {
    const held = await world();
    await held.app.request(
      held.paths.workspace,
      withJson(
        'POST',
        {
          entries: [
            { key: 'SHARED_VAR', value: 'a value both see' },
            { key: 'OTHER_VAR', value: 'another' },
          ],
        },
        held.ownerKey,
      ),
    );

    const byOwner = await held.app.request(held.paths.workspace, as(held.ownerKey));
    const byMember = await held.app.request(held.paths.workspace, as(held.memberKey));

    expect(byMember.status).toBe(byOwner.status);
    expect(await byMember.text()).toBe(await byOwner.text());
  });
});

describe('a variable named after a workspace setting', () => {
  it('changes no setting and is listed as an ordinary variable', async () => {
    const held = await world();
    const settings = Object.keys(WORKSPACE_SETTINGS);
    const before = (await (
      await held.app.request(`${MOUNT}/workspaces/mine`, as(held.ownerKey))
    ).json()) as { settings: Record<string, unknown> };

    const created = await held.app.request(
      held.paths.workspace,
      withJson(
        'POST',
        {
          entries: settings.map((key) => ({
            key: key.toUpperCase(),
            // Values that would be legal for the settings of those names.
            value: key === 'rebuild_strategy' ? 'merge' : 'false',
          })),
        },
        held.ownerKey,
      ),
    );

    const after = (await (
      await held.app.request(`${MOUNT}/workspaces/mine`, as(held.ownerKey))
    ).json()) as { settings: Record<string, unknown> };
    const listed = (await (
      await held.app.request(held.paths.workspace, as(held.ownerKey))
    ).json()) as Array<{ key: string }>;

    expect(created.status).toBe(201);
    expect(after.settings).toEqual(before.settings);
    expect(listed.map((e) => e.key).toSorted()).toEqual(
      settings.map((key) => key.toUpperCase()).toSorted(),
    );
  });
});
