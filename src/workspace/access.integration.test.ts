import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';
import { addMember, removeMember } from '../store/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function sharedWorkspace() {
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

  const ownerKey = (await seedApiKey(store, owner)).presented;
  const memberKey = (await seedApiKey(store, member)).presented;
  await app.request(
    `${MOUNT}/workspaces`,
    withJson(
      'POST',
      { slug: 'shared-project', git_url: 'https://github.com/u/r.git', org_id: org.id },
      ownerKey,
    ),
  );

  return { ...service, owner, org, member, ownerKey, memberKey };
}

describe('a member of the workspace’s organization', () => {
  it('reads exactly what the owner reads, saying only that they are a member', async () => {
    const held = await sharedWorkspace();

    const byOwner = await held.app.request(`${MOUNT}/workspaces/shared-project`, as(held.ownerKey));
    const byMember = await held.app.request(
      `${MOUNT}/workspaces/shared-project`,
      as(held.memberKey),
    );
    const ownerBody = (await byOwner.json()) as Record<string, unknown>;
    const memberBody = (await byMember.json()) as Record<string, unknown>;

    expect(byMember.status).toBe(byOwner.status);
    expect(memberBody['access']).toBe('member');
    expect(ownerBody['access']).toBe('owner');
    expect({ ...memberBody, access: null }).toEqual({ ...ownerBody, access: null });
  });

  it('loses access with the next request once they are removed, with no re-authentication', async () => {
    const held = await sharedWorkspace();
    const read = async () =>
      (await held.app.request(`${MOUNT}/workspaces/shared-project`, as(held.memberKey))).status;

    expect(await read()).toBe(200);

    await removeMember(held.store, held.org.id, held.member.id);

    expect(await read()).toBe(404);

    await addMember(held.store, {
      org_id: held.org.id,
      user_id: held.member.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });

    expect(await read()).toBe(200);
  });
});
