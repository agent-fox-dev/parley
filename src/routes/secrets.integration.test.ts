import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function scopes() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });

  return {
    ...service,
    key: (await seedApiKey(store, owner)).presented,
    paths: {
      user: `${MOUNT}/user/secrets`,
      org: `${MOUNT}/orgs/${org.slug}/secrets`,
      workspace: `${MOUNT}/workspaces/mine/secrets`,
    },
  };
}

const SENTINEL = 'SENTINEL-value-9f3a';

describe('a secret at each scope', () => {
  it.each(['user', 'org', 'workspace'] as const)(
    'is created, listed, replaced and deleted at the %s scope, never showing its value',
    async (family) => {
      const held = await scopes();
      const path = held.paths[family];

      const created = await held.app.request(
        path,
        withJson(
          'POST',
          {
            entries: [
              { key: 'FIRST_KEY', value: SENTINEL },
              { key: 'SECOND_KEY', value: 'another' },
            ],
          },
          held.key,
        ),
      );
      const createdBody = (await created.json()) as Array<Record<string, unknown>>;

      expect(created.status).toBe(201);
      expect(createdBody).toHaveLength(2);
      for (const entry of createdBody) {
        expect(Object.keys(entry).toSorted()).toEqual(['created_at', 'key', 'updated_at']);
      }
      expect(JSON.stringify(createdBody)).not.toContain(SENTINEL);

      const listed = await held.app.request(path, as(held.key));
      const listedBody = (await listed.json()) as Array<{ key: string }>;
      expect(listed.status).toBe(200);
      expect(listedBody.map((e) => e.key)).toEqual(['FIRST_KEY', 'SECOND_KEY']);
      expect(JSON.stringify(listedBody)).not.toContain(SENTINEL);

      const replaced = await held.app.request(
        `${path}/FIRST_KEY`,
        withJson('PATCH', { value: 'replaced-value' }, held.key),
      );
      const replacedBody = (await replaced.json()) as Record<string, unknown>;
      expect(replaced.status).toBe(200);
      expect(Object.keys(replacedBody).toSorted()).toEqual(['created_at', 'key', 'updated_at']);
      expect(JSON.stringify(replacedBody)).not.toContain('replaced-value');

      const removed = await held.app.request(
        `${path}/FIRST_KEY`,
        as(held.key, { method: 'DELETE' }),
      );
      expect(removed.status).toBe(204);

      const after = (await (await held.app.request(path, as(held.key))).json()) as Array<{
        key: string;
      }>;
      expect(after.map((e) => e.key)).toEqual(['SECOND_KEY']);
    },
  );

  it('belongs to its own scope and to no other', async () => {
    const held = await scopes();

    await held.app.request(
      held.paths.user,
      withJson('POST', { entries: [{ key: 'ONLY_MINE', value: 'v' }] }, held.key),
    );

    const inOrg = (await (
      await held.app.request(held.paths.org, as(held.key))
    ).json()) as unknown[];
    const inWorkspace = (await (
      await held.app.request(held.paths.workspace, as(held.key))
    ).json()) as unknown[];

    expect(inOrg).toEqual([]);
    expect(inWorkspace).toEqual([]);
  });
});

describe('a key of any casing', () => {
  it('finds, replaces and deletes the one entry it names', async () => {
    const held = await scopes();
    const path = held.paths.user;
    await held.app.request(
      path,
      withJson('POST', { entries: [{ key: 'MY_KEY', value: 'first' }] }, held.key),
    );

    const duplicate = await held.app.request(
      path,
      withJson('POST', { entries: [{ key: 'my_key', value: 'second' }] }, held.key),
    );
    expect(duplicate.status).toBe(409);

    const replaced = await held.app.request(
      `${path}/My_Key`,
      withJson('PATCH', { value: 'second' }, held.key),
    );
    expect(replaced.status).toBe(200);
    // The entry keeps the casing it was created with.
    expect(((await replaced.json()) as { key: string }).key).toBe('MY_KEY');

    expect(
      (await held.app.request(`${path}/my_key`, as(held.key, { method: 'DELETE' }))).status,
    ).toBe(204);
    expect((await (await held.app.request(path, as(held.key))).json()) as unknown[]).toEqual([]);
  });
});

describe('the hundred entries a scope may hold, over the routes', () => {
  it('fills to the limit, refuses the next, and writes none of an over-full create', async () => {
    const held = await scopes();
    const path = held.paths.user;
    const create = (entries: Array<{ key: string; value: string }>) =>
      held.app.request(path, withJson('POST', { entries }, held.key));

    expect(
      (await create(Array.from({ length: 98 }, (_, i) => ({ key: `K_${String(i)}`, value: 'v' }))))
        .status,
    ).toBe(201);
    expect(
      (
        await create([
          { key: 'NINETY_NINE', value: 'v' },
          { key: 'ONE_HUNDRED', value: 'v' },
        ])
      ).status,
    ).toBe(201);

    expect((await create([{ key: 'ONE_TOO_MANY', value: 'v' }])).status).toBe(400);

    const listed = (await (await held.app.request(path, as(held.key))).json()) as unknown[];
    expect(listed).toHaveLength(100);
  });

  it('writes none of a create of three that would cross the limit', async () => {
    const held = await scopes();
    const path = held.paths.user;
    await held.app.request(
      path,
      withJson(
        'POST',
        { entries: Array.from({ length: 98 }, (_, i) => ({ key: `K_${String(i)}`, value: 'v' })) },
        held.key,
      ),
    );

    const refused = await held.app.request(
      path,
      withJson(
        'POST',
        {
          entries: [
            { key: 'ONE', value: 'v' },
            { key: 'TWO', value: 'v' },
            { key: 'THREE', value: 'v' },
          ],
        },
        held.key,
      ),
    );
    const listed = (await (await held.app.request(path, as(held.key))).json()) as unknown[];

    expect(refused.status).toBe(400);
    expect(listed).toHaveLength(98);
  });
});

describe('a value at the size limit', () => {
  it('is accepted, and one byte more is refused on create and on replace', async () => {
    const held = await scopes();
    const path = held.paths.user;
    const atLimit = 'v'.repeat(262_144);

    expect(
      (
        await held.app.request(
          path,
          withJson('POST', { entries: [{ key: 'BIG_ONE', value: atLimit }] }, held.key),
        )
      ).status,
    ).toBe(201);

    expect(
      (
        await held.app.request(
          path,
          withJson('POST', { entries: [{ key: 'TOO_BIG', value: `${atLimit}v` }] }, held.key),
        )
      ).status,
    ).toBe(400);

    expect(
      (
        await held.app.request(
          `${path}/BIG_ONE`,
          withJson('PATCH', { value: `${atLimit}v` }, held.key),
        )
      ).status,
    ).toBe(400);
  });
});
