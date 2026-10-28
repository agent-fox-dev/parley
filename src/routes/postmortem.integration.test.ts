import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { addMember } from '../store/identity.js';

/**
 * A run has one postmortem, and it is the first one submitted. The
 * second submit is the case worth proving: an agent that retries
 * must find its own account of the run unchanged rather than the
 * retry's.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  return {
    ...service,
    key: (await seedApiKey(service.store, owner)).presented,
    memberKey: (await seedApiKey(service.store, member)).presented,
  };
}

const path = `${MOUNT}/workspaces/mine/runs/${RUN}/postmortem`;

const FIRST = {
  run_status: 'stalled',
  started_at: '2026-07-04T14:30:22Z',
  completed_at: '2026-07-04T15:00:00Z',
  task_summary: { total: 4, done: 3 },
  cost_summary: { input_tokens: 10 },
  blocked_tasks: [{ node_id: 'n-1', reason: 'it waited' }],
  session_history: [{ session_id: 's-1' }],
};

describe('the one postmortem a run has', () => {
  it('is the first submitted, and a later one is answered with it', async () => {
    const held = await world();

    const first = await held.app.request(path, withJson('POST', FIRST, held.key));
    const second = await held.app.request(
      path,
      withJson('POST', { ...FIRST, run_status: 'cost_limit' }, held.key),
    );

    expect(first.status).toBe(201);
    expect(second.status).toBe(200);
    expect(((await first.json()) as { run_status: string }).run_status).toBe('stalled');
    expect(((await second.json()) as { run_status: string }).run_status).toBe('stalled');

    const read = (await (await held.app.request(path, as(held.key))).json()) as Record<
      string,
      unknown
    >;
    expect(read['run_status']).toBe('stalled');
    expect(read['workspace']).toBe('mine');
    expect(read['schema_version']).toBe(1);
    expect(read['task_summary']).toEqual({ total: 4, done: 3 });
    expect(read['blocked_tasks']).toEqual([{ node_id: 'n-1', reason: 'it waited' }]);
    expect(read['session_history']).toEqual([{ session_id: 's-1' }]);
    expect(String(read['ingested_at'])).toMatch(/Z$/);
  });

  it('is read by a member of the organization, who may not submit one', async () => {
    const held = await world();
    await held.app.request(path, withJson('POST', FIRST, held.key));

    const read = await held.app.request(path, as(held.memberKey));
    const written = await held.app.request(path, withJson('POST', FIRST, held.memberKey));

    expect(read.status).toBe(200);
    expect(written.status).toBe(403);
  });
});
