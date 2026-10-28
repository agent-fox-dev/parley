import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The run's postmortem: at most one, and refused rather than half
 * stored. A retrying agent must not replace the account of the run
 * that is already there, which is what makes the second submit a
 * read rather than a write.
 */
const RUN = '20260704_143022_a1b2c3';

/** A postmortem the service accepts, which each case spoils its own way. */
const WHOLE = {
  run_status: 'stalled',
  started_at: '2026-07-04T14:30:22Z',
  completed_at: '2026-07-04T15:00:00Z',
  task_summary: { total: 4 },
  cost_summary: { input_tokens: 10 },
};

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const path = `${MOUNT}/workspaces/mine/runs/${RUN}/postmortem`;

async function stored(held: Awaited<ReturnType<typeof world>>): Promise<number> {
  const rows = await held.store.all<{ n: number }>('SELECT COUNT(*) AS n FROM run_postmortems');
  return Number(rows[0]?.n ?? 0);
}

describe('a postmortem the service refuses', () => {
  it.each([
    ['no run status', { ...WHOLE, run_status: undefined }, 400, undefined],
    ['a run status it does not know', { ...WHOLE, run_status: 'tired' }, 400, undefined],
    ['no task summary', { ...WHOLE, task_summary: undefined }, 400, undefined],
    ['a task summary with no total', { ...WHOLE, task_summary: {} }, 400, undefined],
    ['no cost summary', { ...WHOLE, cost_summary: undefined }, 400, undefined],
    ['a time that is not a time', { ...WHOLE, completed_at: 'later' }, 400, undefined],
    [
      'a schema version it does not know',
      { ...WHOLE, schema_version: 2 },
      422,
      'unknown_schema_version',
    ],
  ])('answers %s with %i, and stores nothing', async (_what, body, status, errorType) => {
    const held = await world();

    const response = await held.app.request(path, withJson('POST', body, held.key));

    expect(response.status).toBe(status);
    if (errorType !== undefined) {
      expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
        errorType,
      );
    }
    expect(await stored(held)).toBe(0);
  });

  it('answers a run that has none with postmortem_not_found', async () => {
    const held = await world();

    const response = await held.app.request(path, as(held.key));

    expect(response.status).toBe(404);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'postmortem_not_found',
    );
  });
});
