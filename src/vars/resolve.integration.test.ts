import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

interface Resolved {
  key: string;
  value: string;
  origin: string;
}

const create = (
  app: Service['app'],
  path: string,
  entries: Array<{ key: string; value: string }>,
  credential: string,
) => app.request(path, withJson('POST', { entries }, credential));

describe('whose user variables the resolved view takes', () => {
  it("takes the workspace owner's, never the caller's", async () => {
    service = await identityService();
    const { store, app } = service;
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
    const ownerKey = (await seedApiKey(store, owner)).presented;
    const memberKey = (await seedApiKey(store, member)).presented;

    await create(app, `${MOUNT}/user/vars`, [{ key: 'OWNERS_VAR', value: 'the owner' }], ownerKey);
    await create(
      app,
      `${MOUNT}/user/vars`,
      [{ key: 'MEMBERS_VAR', value: 'the member' }],
      memberKey,
    );

    const path = `${MOUNT}/workspaces/mine/vars/resolved`;
    const byOwner = await app.request(path, as(ownerKey));
    const byMember = await app.request(path, as(memberKey));
    const resolved = (await byOwner.json()) as Resolved[];

    expect(byMember.status).toBe(200);
    expect(await byMember.text()).toBe(JSON.stringify(resolved));
    expect(resolved.map((e) => e.key)).toEqual(['OWNERS_VAR']);
    expect(resolved[0]?.origin).toBe('user');
  });
});

describe('an organization the workspace owner does not belong to', () => {
  it('contributes nothing to the resolved view, while the other tiers still do', async () => {
    service = await identityService();
    const { store, app } = service;
    const owner = await seedUser(store);
    await seedOrg(store, owner);
    const outsider = await seedUser(store);
    const foreignOrg = await seedOrg(store, outsider);
    // The workspace sits in an organization its owner does not belong to.
    await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: foreignOrg.id });

    const ownerKey = (await seedApiKey(store, owner)).presented;
    const outsiderKey = (await seedApiKey(store, outsider)).presented;

    await create(app, `${MOUNT}/user/vars`, [{ key: 'OWNERS_VAR', value: 'owner' }], ownerKey);
    await create(
      app,
      `${MOUNT}/orgs/${foreignOrg.slug}/vars`,
      [{ key: 'ORGS_VAR', value: 'org' }],
      outsiderKey,
    );
    await create(
      app,
      `${MOUNT}/workspaces/mine/vars`,
      [{ key: 'WORKSPACE_VAR', value: 'workspace' }],
      ownerKey,
    );

    const resolved = (await (
      await app.request(`${MOUNT}/workspaces/mine/vars/resolved`, as(ownerKey))
    ).json()) as Resolved[];

    expect(resolved.map((e) => e.key).toSorted()).toEqual(['OWNERS_VAR', 'WORKSPACE_VAR']);
  });
});
