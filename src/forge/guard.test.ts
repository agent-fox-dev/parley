import { afterEach, describe, expect, it } from 'vitest';
import type { Context } from 'hono';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { workspaceScope } from '../platform/secrets.js';
import {
  type Service,
  MOUNT,
  as,
  identityService,
  recordedEvents,
  withJson,
} from '../testutil/app.js';
import { fakeForge } from '../testutil/fakeforge.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { addMember } from '../store/identity.js';
import { checkedNumber, forgeGuard, reachedForge } from './guard.js';

/**
 * The order a forge route checks in. It is the part of this package
 * most easily got wrong, because every check on its own is obviously
 * right and only their order says whether a caller learns that a
 * workspace exists, or is told about a body on a workspace whose
 * forge nobody can identify.
 *
 * Two routes stand in for the eighteen: they run the guard and
 * nothing else, so what is measured is the order and not a route.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

const forge = fakeForge();

/** A read and a write that do the guard's work and then report. */
function routes(deps: Parameters<typeof forgeGuard>[1]): RouteDefinition[] {
  return [
    {
      path: `${MOUNT}/guarded/:slug/reads/:n`,
      scope: 'forge:read',
      handler: async (c: Context) => {
        const guarded = await forgeGuard(c, deps, 'read');
        const number = checkedNumber(c.req.param('n') ?? '');
        const reached = await reachedForge(deps, guarded);
        return jsonResponse({ number, api_url: reached.target.apiUrl });
      },
    },
    {
      path: `${MOUNT}/guarded/:slug/writes`,
      methods: ['POST'],
      scope: 'forge:write',
      handler: async (c: Context) => {
        const guarded = await forgeGuard(c, deps, 'write');
        const body = (await c.req.json().catch(() => undefined)) as { body?: string } | undefined;
        if (body === undefined || typeof body.body !== 'string') {
          throw new (await import('../http/errors.js')).HttpError(400, 'body is required');
        }
        const reached = await reachedForge(deps, guarded);
        // The body limit is last, after the forge is known.
        if (body.body.length > 10) {
          throw new (await import('../http/errors.js')).HttpError(
            413,
            'body is too long',
            'body_too_large',
          );
        }
        return jsonResponse({ api_url: reached.target.apiUrl });
      },
    },
  ];
}

async function world() {
  // The deps the mounted routes use are filled in once the service
  // that holds the store exists, so the routes and the suite share
  // one store, one secret store and one recorder.
  const deps: Parameters<typeof forgeGuard>[1] = {
    store: undefined as never,
    fetch: forge.fetch,
  };
  service = await identityService(routes(deps));
  const { store } = service;
  deps.store = store;
  deps.secrets = service.secrets;
  deps.events = service.events;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  const member = await seedUser(store);
  await addMember(store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });

  for (const [slug, over] of [
    ['mine', { status: 'ready', git_url: 'https://github.com/acme/widgets.git' }],
    ['retired', { status: 'archived', git_url: 'https://github.com/acme/widgets.git' }],
    ['nameless', { status: 'ready', git_url: 'https://code.acme.test/acme/widgets.git' }],
  ] as const) {
    await seedWorkspace(store, { slug, owner_id: owner.id, org_id: org.id, ...over });
  }
  await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'a-token');

  return {
    ...service,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    readOnlyPat: (await seedPat(store, owner, ['forge:read'])).presented,
    scopelessPat: (await seedPat(store, owner, ['workspaces:read'])).presented,
  };
}

const read = (held: Awaited<ReturnType<typeof world>>, slug: string, n: string, query = '') =>
  `${MOUNT}/guarded/${slug}/reads/${n}${query}`;

describe('what a read route answers first', () => {
  it.each([
    [
      'a token without the scope, even for a slug naming nothing',
      'scopelessPat',
      'nowhere',
      '1',
      '',
      403,
      'forge:read',
    ],
    ['a workspace nobody can see', 'ownerKey', 'nowhere', '1', '', 404, 'workspace not found'],
    [
      'a bad target before a bad path number',
      'ownerKey',
      'mine',
      'not-a-number',
      '?target=sideways',
      400,
      'target',
    ],
    [
      'a bad path number before an unidentifiable forge',
      'ownerKey',
      'nameless',
      'not-a-number',
      '',
      400,
      'positive decimal integer',
    ],
    ['an unidentifiable forge last of all', 'ownerKey', 'nameless', '1', '', 422, 'forge'],
  ])('answers %s', async (_what, credential, slug, n, query, status, message) => {
    const held = await world();

    const response = await held.app.request(
      read(held, slug, n, query),
      as(held[credential as 'ownerKey']),
    );
    const body = (await response.json()) as { error: { message: string; error_type?: string } };

    expect(response.status).toBe(status);
    expect(`${body.error.message} ${body.error.error_type ?? ''}`).toContain(message);
  });

  it('serves a member of the workspace’s organization, and an archived workspace', async () => {
    const held = await world();

    expect((await held.app.request(read(held, 'mine', '1'), as(held.memberKey))).status).toBe(200);
    expect((await held.app.request(read(held, 'retired', '1'), as(held.ownerKey))).status).toBe(
      200,
    );
  });
});

describe('what a write route answers first', () => {
  it.each([
    [
      'a token holding only forge:read, before the workspace is looked up',
      'readOnlyPat',
      'nowhere',
      { body: 'short' },
      403,
      'forge:write',
    ],
    [
      'a member, who may read but not write',
      'memberKey',
      'mine',
      { body: 'short' },
      403,
      'forbidden',
    ],
    [
      'an archived workspace, before the body is read',
      'ownerKey',
      'retired',
      { nothing: true },
      400,
      'archived',
    ],
    ['a body that breaks the route’s rule', 'ownerKey', 'mine', { nothing: true }, 400, 'body'],
    [
      'an unidentifiable forge before an oversized body',
      'ownerKey',
      'nameless',
      { body: 'far longer than the limit' },
      422,
      'forge_unknown',
    ],
    [
      'an oversized body once the forge is known',
      'ownerKey',
      'mine',
      { body: 'far longer than the limit' },
      413,
      'body_too_large',
    ],
  ])('answers %s', async (_what, credential, slug, body, status, message) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/guarded/${slug}/writes`,
      withJson('POST', body, held[credential as 'ownerKey']),
    );
    const answered = (await response.json()) as {
      error: { message: string; error_type?: string };
    };

    expect(response.status).toBe(status);
    expect(`${answered.error.message} ${answered.error.error_type ?? ''}`).toContain(message);
  });
});

describe('a request the guard refused', () => {
  it('reaches no forge and records no event, whichever check refused it', async () => {
    const held = await world();
    const before = forge.calls().length;

    const refusals = [
      held.app.request(read(held, 'nowhere', '1'), as(held.ownerKey)),
      held.app.request(read(held, 'mine', 'x'), as(held.ownerKey)),
      held.app.request(read(held, 'mine', '1', '?target=sideways'), as(held.ownerKey)),
      held.app.request(read(held, 'nameless', '1'), as(held.ownerKey)),
      held.app.request(
        `${MOUNT}/guarded/retired/writes`,
        withJson('POST', { body: 'short' }, held.ownerKey),
      ),
      held.app.request(
        `${MOUNT}/guarded/mine/writes`,
        withJson('POST', { body: 'far longer than the limit' }, held.ownerKey),
      ),
    ];
    for (const refusal of await Promise.all(refusals)) {
      expect(refusal.status).toBeGreaterThanOrEqual(400);
    }

    expect(forge.calls().length).toBe(before);
    expect(await recordedEvents(held.store)).toEqual([]);
  });
});
