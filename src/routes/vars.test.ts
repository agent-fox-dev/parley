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
import { MAX_ENTRIES_PER_SCOPE } from '../entries/validate.js';

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
    org,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    userVars: `${MOUNT}/user/vars`,
    workspaceVars: `${MOUNT}/workspaces/mine/vars`,
  };
}

describe('the rules a variable obeys', () => {
  it.each([
    ['a key with a leading digit', { entries: [{ key: '1BAD', value: 'v' }] }, 400],
    ['a key with a hyphen', { entries: [{ key: 'A-B', value: 'v' }] }, 400],
    ['an empty entry list', { entries: [] }, 400],
    ['an entry with no value', { entries: [{ key: 'A_KEY' }] }, 400],
    [
      'a value over the size limit',
      { entries: [{ key: 'A_KEY', value: 'v'.repeat(262_145) }] },
      400,
    ],
  ])('refuses %s with %i, exactly as a secret create is refused', async (_name, body, expected) => {
    const held = await world();

    const asVariable = await held.app.request(held.userVars, withJson('POST', body, held.ownerKey));
    const asSecret = await held.app.request(
      `${MOUNT}/user/secrets`,
      withJson('POST', body, held.ownerKey),
    );

    expect(asVariable.status).toBe(expected);
    expect(asSecret.status).toBe(expected);
    expect(await held.store.all('SELECT key FROM entries')).toEqual([]);
  });

  it('refuses a duplicate key with 409, writing none of the create', async () => {
    const held = await world();
    await held.app.request(
      held.userVars,
      withJson('POST', { entries: [{ key: 'MY_VAR', value: 'first' }] }, held.ownerKey),
    );

    const again = await held.app.request(
      held.userVars,
      withJson(
        'POST',
        {
          entries: [
            { key: 'FRESH', value: 'v' },
            { key: 'my_var', value: 'second' },
          ],
        },
        held.ownerKey,
      ),
    );
    const listed = (await (
      await held.app.request(held.userVars, as(held.ownerKey))
    ).json()) as Array<{ key: string; value: string }>;

    expect(again.status).toBe(409);
    expect(listed.map((e) => e.key)).toEqual(['MY_VAR']);
    expect(listed[0]?.value).toBe('first');
  });

  it('refuses the entry that would take a scope past its hundred', async () => {
    const held = await world();
    await held.app.request(
      held.userVars,
      withJson(
        'POST',
        {
          entries: Array.from({ length: MAX_ENTRIES_PER_SCOPE }, (_, i) => ({
            key: `K_${String(i)}`,
            value: 'v',
          })),
        },
        held.ownerKey,
      ),
    );

    const refused = await held.app.request(
      held.userVars,
      withJson('POST', { entries: [{ key: 'ONE_TOO_MANY', value: 'v' }] }, held.ownerKey),
    );

    expect(refused.status).toBe(400);
  });

  it('orders a list by key ignoring case', async () => {
    const held = await world();
    await held.app.request(
      held.userVars,
      withJson(
        'POST',
        {
          entries: [
            { key: 'C_KEY', value: 'c' },
            { key: 'a_key', value: 'a' },
            { key: 'B_KEY', value: 'b' },
            { key: 'A_OTHER', value: 'o' },
          ],
        },
        held.ownerKey,
      ),
    );

    const listed = (await (
      await held.app.request(held.userVars, as(held.ownerKey))
    ).json()) as Array<{ key: string }>;

    expect(listed.map((e) => e.key)).toEqual(['a_key', 'A_OTHER', 'B_KEY', 'C_KEY']);
  });

  it('refuses an admin token the user scope, as it refuses it for secrets', async () => {
    const held = await world();

    expect((await held.app.request(held.userVars, as(ADMIN_TOKEN))).status).toBe(403);
  });
});

describe("a member and a workspace's variables", () => {
  it.each([
    ['a create', 'POST', '', { entries: [{ key: 'THEIRS', value: 'v' }] }],
    ['a replace', 'PATCH', '/EXISTING', { value: 'changed' }],
    ['a delete', 'DELETE', '/EXISTING', {}],
  ])('refuses %s with 403 forbidden and changes nothing', async (_name, method, suffix, body) => {
    const held = await world();
    await held.app.request(
      held.workspaceVars,
      withJson('POST', { entries: [{ key: 'EXISTING', value: 'first' }] }, held.ownerKey),
    );

    const response = await held.app.request(
      `${held.workspaceVars}${suffix}`,
      withJson(method, body, held.memberKey),
    );
    const error = (await response.json()) as { error: { message: string; error_type?: string } };
    const listed = (await (
      await held.app.request(held.workspaceVars, as(held.ownerKey))
    ).json()) as Array<{ key: string; value: string }>;

    expect(response.status).toBe(403);
    expect(error.error.error_type).toBe('forbidden');
    expect(error.error.message).toBe('org members have read-only access to this workspace');
    expect(listed).toEqual([expect.objectContaining({ key: 'EXISTING', value: 'first' })]);
  });
});
