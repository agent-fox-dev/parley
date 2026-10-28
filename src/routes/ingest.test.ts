import { afterEach, describe, expect, it } from 'vitest';
import { type Service, ADMIN_TOKEN, MOUNT, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';

/**
 * What an agent may ingest for a run, and what it is told when it may
 * not. The access rule here runs the other way round from the rest of
 * the service — the workspace's status is looked at before who is
 * asking, and a caller who may read the workspace is still refused —
 * so it has a table of its own.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

/** The five single-record routes, with a body each would accept. */
const ROUTES = [
  ['events', { event_type: 'session.start' }],
  ['sessions/outcomes', { session_id: 's-1', status: 'completed' }],
  ['tools/calls', { tool_name: 'bash' }],
  ['tools/errors', { tool_name: 'bash', error_msg: 'it failed' }],
  ['traces', { event_type: 'session.init' }],
] as const;

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

  await seedWorkspace(store, {
    slug: 'active',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  await seedWorkspace(store, {
    slug: 'retired',
    owner_id: owner.id,
    org_id: org.id,
    status: 'archived',
  });

  return {
    ...service,
    ownerKey: (await seedApiKey(store, owner)).presented,
    memberKey: (await seedApiKey(store, member)).presented,
    strangerKey: (await seedApiKey(store, stranger)).presented,
  };
}

/** How many records of every kind the run holds. */
async function stored(held: Awaited<ReturnType<typeof world>>): Promise<number> {
  let total = 0;
  for (const table of [
    'run_events',
    'run_session_outcomes',
    'run_tool_calls',
    'run_tool_errors',
    'run_traces',
  ]) {
    const rows = await held.store.all<{ n: number }>(`SELECT COUNT(*) AS n FROM ${table}`);
    total += Number(rows[0]?.n ?? 0);
  }
  return total;
}

describe('an ingestion the service refuses', () => {
  it.each([
    ['a run id of the wrong shape', 'nonsense', { event_type: 'a.type' }],
    ['a run id with uppercase hex', '20260704_143022_A1B2C3', { event_type: 'a.type' }],
    ['a run id with the wrong length', '20260704_143022_a1b2c', { event_type: 'a.type' }],
    ['a malformed timestamp', RUN, { event_type: 'a.type', timestamp: 'yesterday' }],
    ['an invalid severity', RUN, { event_type: 'a.type', severity: 'loud' }],
    ['a missing event type', RUN, {}],
    [
      'a run id in the body that differs',
      RUN,
      { event_type: 'a.type', run_id: '20260704_143022_ffffff' },
    ],
  ])('answers 400 to %s, and stores nothing', async (_what, run, body) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/${run}/events`,
      withJson('POST', body, held.ownerKey),
    );

    expect(response.status).toBe(400);
    expect(await stored(held)).toBe(0);
  });

  it.each([
    ['a session outcome with no status', 'sessions/outcomes', { session_id: 's-1' }],
    [
      'a session outcome with a negative duration',
      'sessions/outcomes',
      { session_id: 's-1', status: 'completed', duration_ms: -1 },
    ],
    ['a tool call with no tool', 'tools/calls', {}],
    ['a tool error with no message', 'tools/errors', { tool_name: 'bash' }],
    ['a trace of an unknown type', 'traces', { event_type: 'something.else' }],
  ])('answers 400 to %s', async (_what, route, body) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/${RUN}/${route}`,
      withJson('POST', body, held.ownerKey),
    );

    expect(response.status).toBe(400);
    expect(await stored(held)).toBe(0);
  });
});

describe('who may ingest a run’s records', () => {
  it.each([
    ['the owner of an active workspace', 'active', 'ownerKey', 201, undefined],
    ['the owner of an archived workspace', 'retired', 'ownerKey', 409, 'workspace_archived'],
    ['a member of an active workspace', 'active', 'memberKey', 403, 'workspace_access_denied'],
    ['a member of an archived workspace', 'retired', 'memberKey', 409, 'workspace_archived'],
    ['a stranger', 'active', 'strangerKey', 403, 'workspace_access_denied'],
    ['anyone, for a workspace that is not there', 'nowhere', 'ownerKey', 404, undefined],
  ])('answers %s with %i', async (_who, slug, credential, status, errorType) => {
    const held = await world();

    for (const [route, body] of ROUTES) {
      const response = await held.app.request(
        `${MOUNT}/workspaces/${slug}/runs/${RUN}/${route}`,
        withJson('POST', body, held[credential as 'ownerKey']),
      );

      expect(response.status, `${route} answered ${String(response.status)}`).toBe(status);
      if (errorType !== undefined) {
        const answered = (await response.json()) as { error: { error_type?: string } };
        expect(answered.error.error_type).toBe(errorType);
      }
    }
  });

  it('stores nothing for a request it refused', async () => {
    const held = await world();

    await held.app.request(
      `${MOUNT}/workspaces/active/runs/${RUN}/events`,
      withJson('POST', { event_type: 'a.type' }, held.memberKey),
    );
    await held.app.request(
      `${MOUNT}/workspaces/retired/runs/${RUN}/events`,
      withJson('POST', { event_type: 'a.type' }, held.ownerKey),
    );

    expect(await stored(held)).toBe(0);
  });

  it('serves an admin token, which owns every workspace', async () => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/${RUN}/events`,
      withJson('POST', { event_type: 'a.type' }, ADMIN_TOKEN),
    );

    expect(response.status).toBe(201);
  });
});

describe('a batch the service refuses', () => {
  it.each([
    ['an empty array', [], 400],
    [
      'more than a thousand items',
      Array.from({ length: 1001 }, () => ({ event_type: 'a.type' })),
      413,
    ],
  ])('answers %s with %i', async (_what, body, status) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/${RUN}/events/batch`,
      withJson('POST', body, held.ownerKey),
    );

    expect(response.status).toBe(status);
    expect(await stored(held)).toBe(0);
  });

  it('answers 400 to a batch for a run id of the wrong shape', async () => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/nonsense/traces/batch`,
      withJson('POST', [{ event_type: 'session.init' }], held.ownerKey),
    );

    expect(response.status).toBe(400);
  });
});

describe('a record the service accepts', () => {
  it.each(ROUTES)('answers 201 with the identity of the %s it stored', async (route, body) => {
    const held = await world();

    const response = await held.app.request(
      `${MOUNT}/workspaces/active/runs/${RUN}/${route}`,
      withJson('POST', body, held.ownerKey),
    );
    const answered = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(201);
    expect(answered['id']).toMatch(/\S/);
    expect(answered['run_id']).toBe(RUN);
  });

  it('defaults an event’s severity from its type', async () => {
    const held = await world();

    const severities: Record<string, unknown> = {};
    for (const type of ['session.fail', 'git.conflict', 'anything.else']) {
      const response = await held.app.request(
        `${MOUNT}/workspaces/active/runs/${RUN}/events`,
        withJson('POST', { event_type: type }, held.ownerKey),
      );
      severities[type] = ((await response.json()) as { severity: string }).severity;
    }

    expect(severities).toEqual({
      'session.fail': 'error',
      'git.conflict': 'warning',
      'anything.else': 'info',
    });
  });
});
