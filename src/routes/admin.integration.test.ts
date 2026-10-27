import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedOrg, seedPat, seedUser } from '../testutil/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const admin = (init: RequestInit = {}) => as(ADMIN_TOKEN, init);
const post = (init: RequestInit = {}) => admin({ ...init, method: 'POST' });

describe('a user through their whole lifecycle', () => {
  it('is created, listed, read, updated and deleted', async () => {
    service = await identityService();
    const { app } = service;
    const body = {
      username: 'new-person',
      email: 'new-person@example.com',
      provider: 'github',
      provider_id: 'np-1',
    };

    const created = await app.request(`${MOUNT}/users`, withJson('POST', body, ADMIN_TOKEN));
    const user = (await created.json()) as { id: string; username: string };
    expect(created.status).toBe(201);
    expect(user.username).toBe('new-person');

    const duplicate = await app.request(`${MOUNT}/users`, withJson('POST', body, ADMIN_TOKEN));
    expect(duplicate.status).toBe(409);

    const listed = await app.request(`${MOUNT}/users`, admin());
    expect(listed.status).toBe(200);
    expect(((await listed.json()) as Array<{ id: string }>).map((u) => u.id)).toContain(user.id);

    const read = await app.request(`${MOUNT}/users/${user.id}`, admin());
    const etag = read.headers.get('etag');
    expect(read.status).toBe(200);
    expect(etag).not.toBeNull();

    const conditional = await app.request(
      `${MOUNT}/users/${user.id}`,
      admin({ headers: { 'if-none-match': etag ?? '' } }),
    );
    expect(conditional.status).toBe(304);

    const updated = await app.request(
      `${MOUNT}/users/${user.id}`,
      withJson('PATCH', { full_name: 'New Person' }, ADMIN_TOKEN),
    );
    expect(updated.status).toBe(200);
    expect(((await updated.json()) as { full_name: string }).full_name).toBe('New Person');

    const deleted = await app.request(`${MOUNT}/users/${user.id}`, admin({ method: 'DELETE' }));
    expect(deleted.status).toBe(204);

    const after = (await (await app.request(`${MOUNT}/users`, admin())).json()) as Array<{
      id: string;
    }>;
    expect(after.map((u) => u.id)).not.toContain(user.id);
  });
});

describe('promoting, demoting, blocking and unblocking', () => {
  it('take effect, and repeating one changes nothing', async () => {
    service = await identityService();
    const { app, store } = service;
    await seedUser(store, { role: 'admin' });
    const person = await seedUser(store);
    const key = (await seedApiKey(store, person)).presented;

    const role = async () =>
      (
        (await (await app.request(`${MOUNT}/users/${person.id}`, admin())).json()) as {
          role: string;
        }
      ).role;
    const own = async () => (await app.request(`${MOUNT}/user`, as(key))).status;

    expect((await app.request(`${MOUNT}/users/${person.id}/promote`, post())).status).toBe(200);
    expect(await role()).toBe('admin');

    expect((await app.request(`${MOUNT}/users/${person.id}/promote`, post())).status).toBe(200);
    expect(await role()).toBe('admin');

    expect((await app.request(`${MOUNT}/users/${person.id}/demote`, post())).status).toBe(200);
    expect(await role()).toBe('user');

    expect(await own()).toBe(200);
    expect((await app.request(`${MOUNT}/users/${person.id}/block`, post())).status).toBe(200);
    expect(await own()).toBe(401);

    expect((await app.request(`${MOUNT}/users/${person.id}/unblock`, post())).status).toBe(200);
    expect(await own()).toBe(200);
  });
});

describe("another user's credentials", () => {
  it('are listed with their owner and revoked one by one', async () => {
    service = await identityService();
    const { app, store } = service;
    const person = await seedUser(store);
    const key = await seedApiKey(store, person);
    const token = await seedPat(store, person, ['workspaces:read']);

    const keys = (await (
      await app.request(`${MOUNT}/users/${person.id}/keys`, admin())
    ).json()) as Array<Record<string, unknown>>;
    const tokens = (await (
      await app.request(`${MOUNT}/users/${person.id}/tokens`, admin())
    ).json()) as Array<Record<string, unknown>>;

    expect(keys.map((entry) => entry['user_id'])).toEqual([person.id]);
    expect(tokens.map((entry) => entry['user_id'])).toEqual([person.id]);
    expect(JSON.stringify([...keys, ...tokens])).not.toContain(key.record.secret_hash);

    expect(
      (
        await app.request(
          `${MOUNT}/users/${person.id}/keys/${key.record.id}`,
          admin({ method: 'DELETE' }),
        )
      ).status,
    ).toBe(204);
    expect(
      (
        await app.request(
          `${MOUNT}/users/${person.id}/tokens/${token.record.id}`,
          admin({ method: 'DELETE' }),
        )
      ).status,
    ).toBe(204);

    expect((await app.request(`${MOUNT}/user`, as(key.presented))).status).toBe(401);
    expect((await app.request(`${MOUNT}/user`, as(token.presented))).status).toBe(401);
  });
});

describe('an organization through its whole lifecycle', () => {
  it('is created, listed, updated, blocked, unblocked and deleted', async () => {
    service = await identityService();
    const { app, store } = service;
    const owner = await seedUser(store);

    const created = await app.request(
      `${MOUNT}/orgs`,
      withJson(
        'POST',
        {
          name: 'Acme Widgets',
          slug: 'acme-widgets',
          url: 'https://acme.test',
          owner_id: owner.id,
        },
        ADMIN_TOKEN,
      ),
    );
    const org = (await created.json()) as { id: string; slug: string; status: string };
    expect(created.status).toBe(201);
    expect(org.slug).toBe('acme-widgets');

    const listed = await app.request(`${MOUNT}/orgs`, admin());
    expect(listed.status).toBe(200);
    expect(((await listed.json()) as Array<{ id: string }>).map((o) => o.id)).toContain(org.id);

    const updated = await app.request(
      `${MOUNT}/orgs/${org.id}`,
      withJson('PATCH', { name: 'Acme Renamed', url: 'https://renamed.test' }, ADMIN_TOKEN),
    );
    expect(updated.status).toBe(200);
    expect(((await updated.json()) as { name: string }).name).toBe('Acme Renamed');

    expect((await app.request(`${MOUNT}/orgs/${org.id}/block`, post())).status).toBe(200);
    expect((await app.request(`${MOUNT}/orgs/${org.id}/unblock`, post())).status).toBe(200);

    expect((await app.request(`${MOUNT}/orgs/${org.id}`, admin({ method: 'DELETE' }))).status).toBe(
      204,
    );
    expect(((await (await app.request(`${MOUNT}/orgs`, admin())).json()) as unknown[]).length).toBe(
      0,
    );
  });
});

describe('the members of an organization', () => {
  it('are listed, added and removed', async () => {
    service = await identityService();
    const { app, store } = service;
    const owner = await seedUser(store);
    const org = await seedOrg(store, owner);
    const joiner = await seedUser(store);

    const members = async () =>
      (await (await app.request(`${MOUNT}/orgs/${org.id}/members`, admin())).json()) as Array<
        Record<string, unknown>
      >;

    expect((await members()).map((m) => m['user_id'])).toEqual([owner.id]);

    const added = await app.request(
      `${MOUNT}/orgs/${org.id}/members/${joiner.id}`,
      admin({ method: 'PUT' }),
    );
    const membership = (await added.json()) as Record<string, unknown>;

    expect(added.status).toBe(200);
    expect(Object.keys(membership).toSorted()).toEqual([
      'created_at',
      'email',
      'org_id',
      'role',
      'user_id',
      'username',
    ]);
    expect((await members()).map((m) => m['user_id']).toSorted()).toEqual(
      [owner.id, joiner.id].toSorted(),
    );

    const removed = await app.request(
      `${MOUNT}/orgs/${org.id}/members/${joiner.id}`,
      admin({ method: 'DELETE' }),
    );

    expect(removed.status).toBe(204);
    expect((await members()).map((m) => m['user_id'])).toEqual([owner.id]);
  });
});
