import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedBoundToken, seedUser, seedWorkspace } from '../testutil/identity.js';
import { freshStore, seedClock } from '../testutil/identity.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import { queryEvents } from './query.js';
import type { EventRecorder } from './recorder.js';
import { eventRecorder } from './recorder.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

/**
 * A recorder the suite can break. It stands in front of the real one so
 * that the same request can be made with recording working and with it
 * failing, which is the only way to see that the answer is the same.
 */
function breakable() {
  let delegate: EventRecorder | undefined;
  let broken = false;
  const recorder: EventRecorder = {
    record: async (event) => {
      if (broken) {
        throw new Error('the recorder is broken');
      }
      await delegate?.record(event);
    },
    refusal: async (refusal) => {
      if (broken) {
        throw new Error('the recorder is broken');
      }
      await delegate?.refusal(refusal);
    },
  };
  return {
    recorder,
    behind: (real: EventRecorder) => {
      delegate = real;
    },
    breakIt: () => {
      broken = true;
    },
  };
}

/** The suite's chosen name as an entry key, which has its own alphabet. */
const nameKey = (name: string) => name.replaceAll('-', '_').toUpperCase();

/** What two answers must agree on, with the names the suite chose removed. */
async function answer(response: Response, name: string) {
  const text = (await response.text())
    .replaceAll(name, '<name>')
    .replaceAll(nameKey(name), '<NAME>');
  const body: unknown = text.length === 0 ? null : JSON.parse(text);
  const stripped = (value: unknown): unknown => {
    if (Array.isArray(value)) {
      return value.map(stripped);
    }
    if (typeof value === 'object' && value !== null) {
      return Object.fromEntries(
        Object.entries(value as Record<string, unknown>)
          .filter(([key]) => !['id', 'token_id', 'created_at', 'updated_at', 'token'].includes(key))
          .map(([key, held]) => [key, stripped(held)]),
      );
    }
    return value;
  };
  return { status: response.status, body: stripped(body) };
}

describe('a request whose event cannot be recorded', () => {
  it.each([
    [
      'registering a workspace',
      (name: string) => ({
        path: `${MOUNT}/workspaces`,
        init: (key: string) =>
          withJson('POST', { slug: name, git_url: 'https://example.test/r.git' }, key),
      }),
    ],
    [
      'registering a workspace whose slug is taken',
      (_name: string) => ({
        path: `${MOUNT}/workspaces`,
        init: (key: string) =>
          withJson('POST', { slug: 'taken', git_url: 'https://example.test/r.git' }, key),
      }),
    ],
    [
      'changing a workspace',
      (name: string) => ({
        path: `${MOUNT}/workspaces/taken`,
        init: (key: string) => withJson('PATCH', { display_name: name }, key),
      }),
    ],
    [
      'storing a variable',
      (name: string) => ({
        path: `${MOUNT}/user/vars`,
        init: (key: string) =>
          withJson('POST', { entries: [{ key: `VAR_${nameKey(name)}`, value: name }] }, key),
      }),
    ],
    [
      'storing a secret with no key configured',
      (name: string) => ({
        path: `${MOUNT}/user/secrets`,
        init: (key: string) =>
          withJson('POST', { entries: [{ key: `KEY_${nameKey(name)}`, value: name }] }, key),
      }),
    ],
    [
      'minting a bound token',
      (name: string) => ({
        path: `${MOUNT}/workspaces/taken/tokens`,
        init: (key: string) => withJson('POST', { name, permissions: ['workspaces:read'] }, key),
      }),
    ],
    [
      'archiving a workspace that is already archived',
      (_name: string) => ({
        path: `${MOUNT}/workspaces/retired/archive`,
        init: (key: string) => as(key, { method: 'POST' }),
      }),
    ],
  ])('answers %s the same way', async (_what, operation) => {
    const stand = breakable();
    service = await identityService([], { events: stand.recorder });
    stand.behind(eventRecorder({ store: service.store, clock: seedClock() }));
    const owner = await seedUser(service.store);
    const key = (await seedApiKey(service.store, owner)).presented;
    await seedWorkspace(service.store, { slug: 'taken', owner_id: owner.id, status: 'ready' });
    await seedWorkspace(service.store, {
      slug: 'retired',
      owner_id: owner.id,
      status: 'archived',
    });

    const recorded = operation('first-name');
    const working = await service.app.request(recorded.path, recorded.init(key));
    const withWorking = await answer(working, 'first-name');

    stand.breakIt();
    const again = operation('second-name');
    const broken = await service.app.request(again.path, again.init(key));

    expect(await answer(broken, 'second-name')).toEqual(withWorking);
  });

  it('answers a bound token its refusal even when the refusal cannot be recorded', async () => {
    const stand = breakable();
    service = await identityService([], { events: stand.recorder });
    const owner = await seedUser(service.store);
    await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id });
    const bound = await seedBoundToken(service.store, owner, 'mine', ['workspaces:read']);
    stand.breakIt();

    const refused = await service.app.request(`${MOUNT}/workspaces/other`, as(bound.presented));

    expect(refused.status).toBe(404);
  });
});

describe('what the recorder counts', () => {
  it('counts every event it writes, by source and type', async () => {
    const seeded = await freshStore();
    try {
      const metrics = inMemoryMetrics();
      const recorder = eventRecorder({ store: seeded.store, clock: seedClock(), metrics });

      await recorder.record({
        type: 'hub.workspace.archive',
        workspace: 'mine',
        resourceId: 'mine',
        metadata: { head_sha: 'a'.repeat(40) },
      });

      expect(
        metrics
          .snapshot()
          .filter((series) => series.name === 'afhub_audit_events_total')
          .map((series) => [series.labels, series.value]),
      ).toEqual([[{ source: 'hub', event_type: 'hub.workspace.archive' }, 1]]);
    } finally {
      seeded.dispose();
    }
  });
});

describe('an event that belongs to no workspace', () => {
  it('is read by an admin token and by nobody else', async () => {
    const seeded = await freshStore();
    try {
      const recorder = eventRecorder({ store: seeded.store, clock: seedClock() });
      await recorder.record({
        type: 'hub.workspace.create',
        workspace: 'mine',
        resourceId: 'mine',
        metadata: { git_url: 'https://example.test/r.git', branch: 'main' },
      });
      await recorder.record({
        type: 'hub.token.bind',
        workspace: null,
        resourceId: 'a-token',
        metadata: { token_id: 'a-token', workspace: null, permissions: [], expires_at: null },
      });

      // Through the query the routes use, because the rule is about
      // what a caller is shown and not about a clause.
      const read = async (visible: string[] | 'every') =>
        queryEvents(seeded.store, visible, {}, { limit: 50, order: 'desc' });

      expect((await read(['mine'])).map((one) => one.workspace)).toEqual(['mine']);
      expect((await read('every')).map((one) => one.workspace).toSorted()).toEqual(['mine', null]);
    } finally {
      seeded.dispose();
    }
  });
});
