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
import { addMember } from '../store/identity.js';

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
  const stranger = await seedUser(store);
  await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });

  return {
    ...service,
    owner,
    org,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    strangerKey: (await seedApiKey(store, stranger)).presented,
  };
}

const scopes = (org: string) => ({
  user: `${MOUNT}/user/secrets`,
  org: `${MOUNT}/orgs/${org}/secrets`,
  workspace: `${MOUNT}/workspaces/mine/secrets`,
});

describe('who may reach each secret scope', () => {
  it('refuses an admin token on the user scope, because it has no user', async () => {
    const held = await world();

    const response = await held.app.request(scopes(held.org.slug).user, as(ADMIN_TOKEN));

    expect(response.status).toBe(403);
  });

  it('refuses a non-member the organization scope, as it refuses an unknown organization', async () => {
    const held = await world();

    const nonMember = await held.app.request(scopes(held.org.slug).org, as(held.strangerKey));
    const unknown = await held.app.request(`${MOUNT}/orgs/no-such-org/secrets`, as(held.ownerKey));

    expect(nonMember.status).toBe(404);
    expect(await nonMember.text()).toBe(await unknown.text());
  });

  it('serves the organization scope to a member and to an admin token', async () => {
    const held = await world();

    expect((await held.app.request(scopes(held.org.slug).org, as(held.memberKey))).status).toBe(
      200,
    );
    expect((await held.app.request(scopes(held.org.slug).org, as(ADMIN_TOKEN))).status).toBe(200);
  });

  it('refuses a member and a stranger the workspace scope alike', async () => {
    const held = await world();

    const byMember = await held.app.request(scopes(held.org.slug).workspace, as(held.memberKey));
    const byStranger = await held.app.request(
      scopes(held.org.slug).workspace,
      as(held.strangerKey),
    );

    expect(byMember.status).toBe(404);
    expect(await byMember.text()).toBe(await byStranger.text());
  });

  it('serves the workspace scope to its owner and to an admin token', async () => {
    const held = await world();

    expect(
      (await held.app.request(scopes(held.org.slug).workspace, as(held.ownerKey))).status,
    ).toBe(200);
    expect((await held.app.request(scopes(held.org.slug).workspace, as(ADMIN_TOKEN))).status).toBe(
      200,
    );
  });

  it('answers 404 for a workspace that does not exist', async () => {
    const held = await world();

    expect(
      (await held.app.request(`${MOUNT}/workspaces/never-was/secrets`, as(held.ownerKey))).status,
    ).toBe(404);
  });
});

describe('a secret create the service refuses', () => {
  it('answers 409 for a key the scope already holds, writing none of the entries', async () => {
    const held = await world();
    const path = scopes(held.org.slug).user;
    await held.app.request(
      path,
      withJson('POST', { entries: [{ key: 'my_key', value: 'first' }] }, held.ownerKey),
    );

    const again = await held.app.request(
      path,
      withJson(
        'POST',
        {
          entries: [
            { key: 'FRESH_KEY', value: 'v' },
            { key: 'MY_KEY', value: 'second' },
          ],
        },
        held.ownerKey,
      ),
    );
    const listed = (await (await held.app.request(path, as(held.ownerKey))).json()) as Array<{
      key: string;
    }>;

    expect(again.status).toBe(409);
    expect(listed.map((e) => e.key)).toEqual(['my_key']);
  });
});

async function withOne() {
  const held = await world();
  const path = scopes(held.org.slug).user;
  await held.app.request(
    path,
    withJson('POST', { entries: [{ key: 'THE_KEY', value: 'the value' }] }, held.ownerKey),
  );
  return { held, path };
}

describe('a secret change the service refuses', () => {
  it('answers 400 for a replace with no value', async () => {
    const { held, path } = await withOne();

    const response = await held.app.request(
      `${path}/THE_KEY`,
      withJson('PATCH', {}, held.ownerKey),
    );

    expect(response.status).toBe(400);
  });

  it.each([
    ['a replace', 'PATCH'],
    ['a delete', 'DELETE'],
  ])('answers 404 for %s of a key the scope lacks', async (_name, method) => {
    const { held, path } = await withOne();

    const response = await held.app.request(
      `${path}/ABSENT_KEY`,
      withJson(method, { value: 'v' }, held.ownerKey),
    );
    const listed = (await (await held.app.request(path, as(held.ownerKey))).json()) as unknown[];

    expect(response.status).toBe(404);
    expect(listed).toHaveLength(1);
  });
});
