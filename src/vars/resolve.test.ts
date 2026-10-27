import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

interface Resolved {
  key: string;
  value: string;
  origin: string;
}

async function world() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });

  return {
    ...service,
    owner,
    org,
    ownerKey: (await seedApiKey(store, owner)).presented,
    resolved: `${MOUNT}/workspaces/mine/vars/resolved`,
  };
}

const put = (
  held: Awaited<ReturnType<typeof world>>,
  path: string,
  entries: Array<{ key: string; value: string }>,
  credential: string,
) => held.app.request(path, withJson('POST', { entries }, credential));

async function readResolved(
  held: Awaited<ReturnType<typeof world>>,
  credential: string,
): Promise<Resolved[]> {
  const response = await held.app.request(held.resolved, as(credential));
  expect(response.status).toBe(200);
  return (await response.json()) as Resolved[];
}

describe('the resolved view', () => {
  it('lets each tier override the one outside it, and says where each value came from', async () => {
    const held = await world();
    await put(
      held,
      `${MOUNT}/user/vars`,
      [
        { key: 'ALL_THREE', value: 'from the user' },
        { key: 'USER_ONLY', value: 'user only' },
      ],
      held.ownerKey,
    );
    await put(
      held,
      `${MOUNT}/orgs/${held.org.slug}/vars`,
      [
        { key: 'ALL_THREE', value: 'from the org' },
        { key: 'ORG_AND_WORKSPACE', value: 'from the org' },
        { key: 'ORG_ONLY', value: 'org only' },
      ],
      held.ownerKey,
    );
    await put(
      held,
      `${MOUNT}/workspaces/mine/vars`,
      [
        { key: 'ALL_THREE', value: 'from the workspace' },
        { key: 'ORG_AND_WORKSPACE', value: 'from the workspace' },
        { key: 'WORKSPACE_ONLY', value: 'workspace only' },
      ],
      held.ownerKey,
    );

    const resolved = await readResolved(held, held.ownerKey);
    const byKey = new Map(resolved.map((entry) => [entry.key, entry]));

    expect(resolved).toHaveLength(5);
    expect(byKey.get('ALL_THREE')).toMatchObject({
      value: 'from the workspace',
      origin: 'workspace',
    });
    expect(byKey.get('ORG_AND_WORKSPACE')).toMatchObject({
      value: 'from the workspace',
      origin: 'workspace',
    });
    expect(byKey.get('ORG_ONLY')).toMatchObject({ value: 'org only', origin: 'org' });
    expect(byKey.get('USER_ONLY')).toMatchObject({ value: 'user only', origin: 'user' });
    expect(byKey.get('WORKSPACE_ONLY')).toMatchObject({
      value: 'workspace only',
      origin: 'workspace',
    });
  });

  it('is ordered by key ignoring case, whatever tier each value came from', async () => {
    const held = await world();
    await put(held, `${MOUNT}/user/vars`, [{ key: 'b_user', value: 'u' }], held.ownerKey);
    await put(
      held,
      `${MOUNT}/orgs/${held.org.slug}/vars`,
      [{ key: 'A_ORG', value: 'o' }],
      held.ownerKey,
    );
    await put(
      held,
      `${MOUNT}/workspaces/mine/vars`,
      [
        { key: 'a_workspace', value: 'w' },
        { key: 'C_WORKSPACE', value: 'w' },
      ],
      held.ownerKey,
    );

    const resolved = await readResolved(held, held.ownerKey);

    expect(resolved.map((entry) => entry.key)).toEqual([
      'A_ORG',
      'a_workspace',
      'b_user',
      'C_WORKSPACE',
    ]);
  });
});
