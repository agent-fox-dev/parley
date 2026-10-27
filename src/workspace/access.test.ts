import { afterEach, describe, expect, it } from 'vitest';
import type { Context } from 'hono';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedOrg, seedPat, seedUser } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';
import { SEED_NOW } from '../testutil/identity.js';
import type { RouteDefinition } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { jsonResponse } from '../http/json.js';
import { type AccessNeed, hidesMissingScope, workspaceAccess } from './access.js';

let service: Service;
let handlerCalls = 0;

afterEach(() => {
  service.dispose();
  handlerCalls = 0;
});

/** One route per level, so the decision is reached exactly as a route reaches it. */
function levelRoutes(store: () => RelationalStore): RouteDefinition[] {
  const route = (need: AccessNeed): RouteDefinition => ({
    path: `${MOUNT}/probe/${need}/:slug`,
    methods: ['GET', 'POST'],
    scope: need === 'read' ? 'workspaces:read' : 'workspaces:write',
    handler: async (c: Context) => {
      const { workspace, level } = await workspaceAccess(c, store(), need);
      handlerCalls += 1;
      return jsonResponse({ slug: workspace.slug, level });
    },
  });
  return [route('read'), route('write'), route('private')];
}

async function world() {
  let relational: RelationalStore | undefined;
  service = await identityService(levelRoutes(() => relational as RelationalStore));
  relational = service.store;
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

  await store.run(
    `INSERT INTO workspaces (slug, git_url, owner_id, org_id, status, display_name, description,
       workspace_mode, settings, created_at, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      'the-workspace',
      'https://github.com/u/r.git',
      owner.id,
      org.id,
      'ready',
      'the-workspace',
      '',
      'standard',
      '{}',
      SEED_NOW.toISOString(),
      SEED_NOW.toISOString(),
    ],
  );

  return {
    ...service,
    owner,
    org,
    member,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    strangerKey: (await seedApiKey(store, stranger)).presented,
  };
}

const probe = (need: AccessNeed, slug = 'the-workspace') => `${MOUNT}/probe/${need}/${slug}`;

describe('the level one caller has towards one workspace', () => {
  it.each(['read', 'write', 'private'] as const)(
    'is the same whichever %s-level route asks',
    async (need) => {
      const held = await world();

      const owner = await held.app.request(probe(need), as(held.ownerKey));
      const admin = await held.app.request(probe(need), as(ADMIN_TOKEN));

      expect(((await owner.json()) as { level: string }).level).toBe('owner');
      expect(((await admin.json()) as { level: string }).level).toBe('admin');
    },
  );

  it('is member for a user in the organization who does not own the workspace', async () => {
    const held = await world();

    const response = await held.app.request(probe('read'), as(held.memberKey));

    expect(((await response.json()) as { level: string }).level).toBe('member');
  });
});

describe("a member's answer", () => {
  it('is the owner’s on a read-level route', async () => {
    const held = await world();

    const owner = await held.app.request(probe('read'), as(held.ownerKey));
    const member = await held.app.request(probe('read'), as(held.memberKey));

    expect(member.status).toBe(owner.status);
    expect(((await member.json()) as { slug: string }).slug).toBe('the-workspace');
  });

  it('is 403 forbidden with the documented message on a write-level route', async () => {
    const held = await world();

    const response = await held.app.request(probe('write'), as(held.memberKey));
    const body = (await response.json()) as { error: { message: string; error_type?: string } };

    expect(response.status).toBe(403);
    expect(body.error.error_type).toBe('forbidden');
    expect(body.error.message).toBe('org members have read-only access to this workspace');
    expect(handlerCalls).toBe(0);
  });

  it('is a plain 404 on a private-level route', async () => {
    const held = await world();

    const response = await held.app.request(probe('private'), as(held.memberKey));

    expect(response.status).toBe(404);
    expect(((await response.json()) as { error: { message: string } }).error.message).toBe(
      'workspace not found',
    );
  });
});

describe('what a caller with no access is told', () => {
  it('cannot be told apart from a workspace that does not exist', async () => {
    const held = await world();

    const answers = await Promise.all([
      held.app.request(probe('private'), as(held.memberKey)),
      held.app.request(probe('read'), as(held.strangerKey)),
      held.app.request(probe('read', 'no-such-workspace'), as(held.ownerKey)),
    ]);
    const seen = await Promise.all(
      answers.map(async (r) => ({ status: r.status, body: await r.text() })),
    );

    expect(seen[0]).toEqual(seen[1]);
    expect(seen[1]).toEqual(seen[2]);
    expect(seen[0]?.status).toBe(404);
    expect(handlerCalls).toBe(0);
  });
});

describe('the order the checks run in', () => {
  it('decides access before the route looks at the workspace, and the scope before that', async () => {
    const held = await world();
    await held.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
      'archived',
      'the-workspace',
    ]);
    const unscoped = (await seedPat(held.store, held.member, ['patches:read'])).presented;
    const scopedMember = (await seedPat(held.store, held.member, ['workspaces:write'])).presented;

    const byMember = await held.app.request(probe('write'), as(scopedMember));
    const byUnscoped = await held.app.request(probe('write'), as(unscoped));
    const byNobody = await held.app.request(probe('write'));

    expect(byMember.status).toBe(403);
    expect(((await byMember.json()) as { error: { error_type?: string } }).error.error_type).toBe(
      'forbidden',
    );
    expect(byUnscoped.status).toBe(403);
    expect(byNobody.status).toBe(401);
  });
});

describe('an access lookup the store cannot answer', () => {
  it('refuses with 500 and grants nothing', async () => {
    const held = await world();
    await held.store.run('DROP TABLE workspaces');

    const answers = await Promise.all([
      held.app.request(probe('read'), as(held.ownerKey)),
      held.app.request(probe('write'), as(held.ownerKey)),
      held.app.request(probe('read'), as(held.memberKey)),
      held.app.request(probe('read'), as(held.strangerKey)),
    ]);

    expect(answers.map((r) => r.status)).toEqual([500, 500, 500, 500]);
    expect(handlerCalls).toBe(0);
  });
});

describe('which routes hide a missing scope', () => {
  it.each([
    ['GET', '/workspaces', true],
    ['GET', '/workspaces/:slug', true],
    ['PATCH', '/workspaces/:slug', true],
    ['DELETE', '/workspaces/:slug', true],
    ['POST', '/workspaces/:slug/archive', true],
    ['POST', '/workspaces/:slug/reactivate', true],
    ['POST', '/workspaces', false],
    ['POST', '/workspaces/:slug/tokens', false],
    ['GET', '/workspaces/:slug/tokens', false],
    ['POST', '/workspaces/:slug/sandboxes', false],
  ])('%s %s hides it: %s', (method, path, hidden) => {
    expect(hidesMissingScope(method, path)).toBe(hidden);
  });

  it('answers a hidden refusal exactly as a workspace that does not exist', async () => {
    const held = await world();
    const unscoped = (await seedPat(held.store, held.owner, ['patches:read'])).presented;

    const hiddenRefusal = await held.app.request(`${MOUNT}/workspaces/the-workspace`, as(unscoped));
    const absent = await held.app.request(
      `${MOUNT}/workspaces/no-such-workspace`,
      as(held.ownerKey),
    );

    expect(hiddenRefusal.status).toBe(404);
    expect(await hiddenRefusal.text()).toBe(await absent.text());
  });

  it('answers the create with the ordinary refusal and no error_type', async () => {
    const held = await world();
    const unscoped = (await seedPat(held.store, held.owner, ['patches:read'])).presented;

    const response = await held.app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', { slug: 'another-one', git_url: 'https://github.com/u/r.git' }, unscoped),
    );
    const body = (await response.json()) as { error: Record<string, unknown> };

    expect(response.status).toBe(403);
    expect(Object.keys(body.error)).not.toContain('error_type');
  });
});
