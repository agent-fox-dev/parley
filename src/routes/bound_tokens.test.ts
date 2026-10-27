import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import {
  SEED_NOW,
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { addMember } from '../store/identity.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

const mintBody = { name: 'agent-fox service', permissions: ['workspaces:read'] };
const mint = (slug: string) => `${MOUNT}/workspaces/${slug}/tokens`;

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

  const mine = await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });
  const archived = await seedWorkspace(store, {
    slug: 'retired',
    owner_id: owner.id,
    org_id: org.id,
    status: 'archived',
  });
  const theirs = await seedWorkspace(store, { slug: 'theirs', owner_id: stranger.id });

  return {
    ...service,
    owner,
    member,
    mine,
    archived,
    theirs,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    strangerKey: (await seedApiKey(store, stranger)).presented,
    bound: (await seedBoundToken(store, owner, 'mine', ['workspaces:read', 'tokens:manage']))
      .presented,
  };
}

describe('who may mint a token bound to a workspace', () => {
  it('admits the owner', async () => {
    const held = await world();

    expect(
      (await held.app.request(mint('mine'), withJson('POST', mintBody, held.ownerKey))).status,
    ).toBe(201);
  });

  it.each([
    ['a token already bound to the workspace', 'bound', 403, 'token_bound'],
    ['the deployment admin token', 'admin', 403, undefined],
    ['a member of the organization', 'memberKey', 404, undefined],
    ['a stranger', 'strangerKey', 404, undefined],
  ] as const)('refuses %s with %i', async (_name, who, expected, errorType) => {
    const held = await world();
    const credential = who === 'admin' ? ADMIN_TOKEN : held[who];

    const response = await held.app.request(mint('mine'), withJson('POST', mintBody, credential));
    const body = (await response.json()) as { error: { error_type?: string } };

    expect(response.status).toBe(expected);
    expect(body.error.error_type).toBe(errorType);
    expect(await held.store.all('SELECT token_id FROM token_bindings')).toHaveLength(1);
  });

  it('refuses an archived workspace with 400 and binds nothing', async () => {
    const held = await world();

    const response = await held.app.request(
      mint('retired'),
      withJson('POST', mintBody, held.ownerKey),
    );

    expect(response.status).toBe(400);
    expect(await held.store.all('SELECT token_id FROM token_bindings')).toHaveLength(1);
  });

  it('passes the token creation’s own refusals through unchanged', async () => {
    const held = await world();

    const noName = await held.app.request(
      mint('mine'),
      withJson('POST', { permissions: ['workspaces:read'] }, held.ownerKey),
    );
    const badScope = await held.app.request(
      mint('mine'),
      withJson('POST', { name: 'a token', permissions: ['workspaces:create'] }, held.ownerKey),
    );

    expect(noName.status).toBe(400);
    expect(badScope.status).toBe(400);
    expect(await held.store.all('SELECT token_id FROM token_bindings')).toHaveLength(1);
  });
});

describe('a bound token on a route naming another workspace', () => {
  it('is told that workspace does not exist, whatever its real state', async () => {
    const held = await world();

    const answers = await Promise.all([
      held.app.request(`${MOUNT}/workspaces/theirs`, as(held.bound)),
      held.app.request(`${MOUNT}/workspaces/retired`, as(held.bound)),
      held.app.request(`${MOUNT}/workspaces/never-existed`, as(held.bound)),
    ]);
    const seen = await Promise.all(
      answers.map(async (r) => ({ status: r.status, body: await r.text() })),
    );

    expect(seen.every((s) => s.status === 404)).toBe(true);
    expect(seen[0]).toEqual(seen[1]);
    expect(seen[1]).toEqual(seen[2]);
    expect(JSON.parse(seen[0]?.body ?? '{}')).toEqual({
      error: { code: 404, message: 'workspace not found' },
    });
  });

  it('is told so even where a missing scope would otherwise answer 403', async () => {
    const held = await world();
    const narrow = (await seedBoundToken(held.store, held.owner, 'mine', ['patches:read']))
      .presented;

    const response = await held.app.request(
      `${MOUNT}/workspaces/theirs`,
      withJson('PATCH', { display_name: 'x' }, narrow),
    );

    expect(response.status).toBe(404);
  });

  it('reaches its own workspace as an unbound token of the same scopes would', async () => {
    const held = await world();

    expect((await held.app.request(`${MOUNT}/workspaces/mine`, as(held.bound))).status).toBe(200);
  });
});

describe('a bound token on a route no bound token may use', () => {
  it.each([
    ['the workspace create', 'POST', '/workspaces', 'workspace mine'],
    ['a path under the mount that matches no route', 'GET', '/user/secrets', 'a workspace'],
    ['the sandbox list', 'GET', '/sandboxes', 'workspace mine'],
    ['an invented path under the mount', 'GET', '/nothing-here', 'workspace mine'],
    ['the profile route', 'GET', '/user', 'a workspace'],
    ['the token list', 'GET', '/user/tokens', 'a workspace'],
    ['the user administration list', 'GET', '/users', 'a workspace'],
    ['an organization read', 'GET', '/orgs/anything', 'a workspace'],
  ])('refuses %s with 403 token_bound', async (_name, method, path, says) => {
    const held = await world();

    const response = await held.app.request(`${MOUNT}${path}`, as(held.bound, { method }));
    const body = (await response.json()) as { error: { message: string; error_type?: string } };

    expect(response.status).toBe(403);
    expect(body.error.error_type).toBe('token_bound');
    expect(body.error.message).toContain(says);
  });

  it('names the workspace on a hub route and does not on an identity one', async () => {
    const held = await world();

    const hub = await held.app.request(`${MOUNT}/sandboxes`, as(held.bound));
    const identity = await held.app.request(`${MOUNT}/user`, as(held.bound));

    expect(((await hub.json()) as { error: { message: string } }).error.message).toBe(
      'this token is bound to workspace mine',
    );
    expect(((await identity.json()) as { error: { message: string } }).error.message).toBe(
      'this token is bound to a workspace',
    );
  });

  it('may still read its own workspace list', async () => {
    const held = await world();

    expect((await held.app.request(`${MOUNT}/workspaces`, as(held.bound))).status).toBe(200);
  });
});

describe('a binding the store cannot read', () => {
  it('refuses every request of that token rather than treating it as unbound', async () => {
    const held = await world();
    await held.store.run('DROP TABLE token_bindings');

    const answers = await Promise.all([
      held.app.request(`${MOUNT}/workspaces/mine`, as(held.bound)),
      held.app.request(`${MOUNT}/workspaces/theirs`, as(held.bound)),
      held.app.request(`${MOUNT}/user`, as(held.bound)),
    ]);

    expect(answers.map((r) => r.status)).toEqual([500, 500, 500]);
  });
});

describe('the workspace list a bound token sees', () => {
  it('holds at most its own workspace', async () => {
    const held = await world();
    const alsoMine = await seedWorkspace(held.store, {
      slug: 'also-mine',
      owner_id: held.owner.id,
    });

    const unbound = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.ownerKey))
    ).json()) as Array<{ slug: string }>;
    const bound = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(held.bound))
    ).json()) as Array<{ slug: string }>;

    expect(unbound.map((w) => w.slug)).toContain(alsoMine.slug);
    expect(bound.map((w) => w.slug)).toEqual(['mine']);
  });
});
