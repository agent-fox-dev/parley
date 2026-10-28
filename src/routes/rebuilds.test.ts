import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { newIdentifier } from '../auth/credentials.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { insertRebuild, updateRebuild } from '../rebuild/record.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPatch,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';

/**
 * The rebuild routes. A rebuild is a job, so most of what these
 * answer is about a record rather than about git; what matters is
 * that the preconditions are exact and that a client can follow a job
 * from queued to done.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
  base: string;
}

async function world(over: object = {}): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-rebuilds-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, jobs: 'store' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    branch: 'main',
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'upstream\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);

  return {
    service,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

async function call(
  place: World,
  path: string,
  init: { method?: string; body?: unknown } = {},
): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}${path}`, {
    method: init.method ?? 'GET',
    headers: {
      authorization: `Bearer ${place.credential}`,
      ...(init.body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(init.body === undefined ? {} : { body: JSON.stringify(init.body) }),
  });
}

/** A patch with a branch, so a rebuild has something to do. */
async function patch(place: World, branch: string, files: Record<string, string>): Promise<void> {
  const built = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'upstream\n', ...files },
    [place.base],
    `on ${branch}`,
  );
  await place.repos.updateRefs(place.repository, [
    { name: `refs/heads/${branch}`, oldOid: null, newOid: built.oid, force: true },
  ]);
  await seedPatch(place.service.store, place.slug, branch);
}

/**
 * A job record in whatever state the suite needs to see refused.
 * Each one is queued a second after the last, because "newest first"
 * needs them to have been queued at different times to mean anything.
 */
let queuedAt = SEED_NOW.getTime();

async function jobIn(place: World, status: string, over: object = {}): Promise<string> {
  const id = newIdentifier();
  queuedAt += 1000;
  await insertRebuild(
    place.service.store,
    {
      id,
      workspace_slug: place.slug,
      strategy: 'rebase',
      fail_mode: 'fail_fast',
      submitted_by: 'u-1',
    },
    new Date(queuedAt).toISOString(),
  );
  await updateRebuild(place.service.store, id, { status: status as 'queued', ...over });
  return id;
}

describe('submitting a rebuild', () => {
  it('queues it with the strategy and fail mode the body names', async () => {
    const place = await world();
    await patch(place, 'one', { 'one.txt': 'first\n' });

    const response = await call(place, '/rebuild', {
      method: 'POST',
      body: { strategy: 'merge', fail_mode: 'continue' },
    });
    const queued = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(202);
    expect(queued['type']).toBe('rebuild');
    expect(queued['status']).toBe('queued');
    expect(queued['key']).toBe(place.slug);
    expect(queued['group_key']).toBe(`${place.slug}:integration`);
    expect((queued['payload'] as Record<string, unknown>)['strategy']).toBe('merge');

    const read = (await (await call(place, `/rebuilds/${String(queued['id'])}`)).json()) as Record<
      string,
      unknown
    >;
    expect(read['strategy']).toBe('merge');
  });

  it('takes the workspace settings where the body says nothing', async () => {
    const place = await world({
      settings: { rebuild_strategy: 'merge', rebuild_fail_mode: 'continue' },
    });
    await patch(place, 'one', { 'one.txt': 'first\n' });

    const response = await call(place, '/rebuild', { method: 'POST' });
    const id = ((await response.json()) as { id: string }).id;

    expect(response.status).toBe(202);
    const read = (await (await call(place, `/rebuilds/${id}`)).json()) as Record<string, unknown>;
    expect(read['strategy']).toBe('merge');
  });

  it.each([
    [
      'a standard workspace',
      { workspace_mode: 'standard' as const },
      400,
      'workspace_mode_mismatch',
    ],
    ['an unready workspace', { status: 'pending' as const }, 400, undefined],
  ])('is refused for %s', async (_what, over, status, type) => {
    const place = await world(over);
    await patch(place, 'one', { 'one.txt': 'first\n' });

    const response = await call(place, '/rebuild', { method: 'POST' });

    expect(response.status).toBe(status);
    if (type !== undefined) {
      expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
        type,
      );
    }
  });

  it('is refused when no patch is there to apply', async () => {
    const place = await world();
    await seedPatch(place.service.store, place.slug, 'switched-off', { status: 'disabled' });

    const response = await call(place, '/rebuild', { method: 'POST' });

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'no_active_patches',
    );
  });

  it.each([
    ['a strategy nobody has', { strategy: 'teleport' }],
    ['a fail mode nobody has', { fail_mode: 'shrug' }],
    ['a body that is not an object', 'rebuild please'],
  ])('is refused for %s', async (_what, body) => {
    const place = await world();
    await patch(place, 'one', { 'one.txt': 'first\n' });

    expect((await call(place, '/rebuild', { method: 'POST', body })).status).toBe(400);
  });

  it('is refused while another rebuild of the same workspace is under way', async () => {
    const place = await world();
    await patch(place, 'one', { 'one.txt': 'first\n' });
    await jobIn(place, 'running');

    const response = await call(place, '/rebuild', { method: 'POST' });

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'concurrent_rebuild',
    );
    const listed = (await (await call(place, '/rebuilds')).json()) as unknown[];
    expect(listed).toHaveLength(1);
  });
});

describe('reading rebuild jobs', () => {
  it('lists them newest first and reads one with its results', async () => {
    const place = await world();
    const older = await jobIn(place, 'completed');
    const newer = await jobIn(place, 'running', {
      results: [
        { patch_id: 'p-1', branch_name: 'one', position: 1, outcome: 'applied' },
        {
          patch_id: 'p-2',
          branch_name: 'two',
          position: 2,
          outcome: 'skipped',
          reason: 'disabled',
        },
      ],
    });

    const listed = (await (await call(place, '/rebuilds')).json()) as Array<
      Record<string, unknown>
    >;
    expect(listed.map((one) => one['id'])).toEqual([newer, older]);

    const read = (await (await call(place, `/rebuilds/${newer}`)).json()) as Record<
      string,
      unknown
    >;
    const results = read['patch_results'] as Array<Record<string, unknown>>;
    expect(results.map((one) => one['status'])).toEqual(['success', 'skipped']);
    expect(results[1]?.['skipped_reason']).toBe('disabled');
  });

  it('answers 404 for an unknown job and for one of another workspace', async () => {
    const place = await world();
    const elsewhere = await world();
    const theirs = await jobIn(elsewhere, 'completed');

    expect((await call(place, '/rebuilds/nobody')).status).toBe(404);
    expect((await call(place, `/rebuilds/${theirs}`)).status).toBe(404);
  });
});

describe('calling a rebuild off and queueing it again', () => {
  it('cancels a queued job and refuses every other status', async () => {
    const place = await world();
    const queued = await jobIn(place, 'queued');

    const cancelled = await call(place, `/rebuilds/${queued}`, { method: 'DELETE' });
    expect(cancelled.status).toBe(200);
    expect(((await cancelled.json()) as { status: string }).status).toBe('cancelled');

    for (const status of ['running', 'completed', 'failed', 'cancelled']) {
      const other = await jobIn(place, status);
      expect((await call(place, `/rebuilds/${other}`, { method: 'DELETE' })).status).toBe(409);
    }
  });

  it('queues a dead-lettered job again and refuses every other status', async () => {
    const place = await world();
    const dead = await jobIn(place, 'dead_letter', { error: 'it went wrong' });

    const again = await call(place, `/rebuilds/${dead}/requeue`, { method: 'POST' });
    expect(again.status).toBe(200);
    const body = (await again.json()) as Record<string, unknown>;
    expect(body['status']).toBe('queued');
    expect(body['error']).toBeUndefined();

    const completed = await jobIn(place, 'completed');
    expect((await call(place, `/rebuilds/${completed}/requeue`, { method: 'POST' })).status).toBe(
      409,
    );
  });

  it('refuses to queue one again while another is under way', async () => {
    const place = await world();
    const dead = await jobIn(place, 'dead_letter');
    await jobIn(place, 'running');

    const response = await call(place, `/rebuilds/${dead}/requeue`, { method: 'POST' });

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'concurrent_rebuild',
    );
  });
});

describe('undoing a rebuild', () => {
  it('sets the integration branch back where it was', async () => {
    const place = await world();
    const before = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'upstream\n', 'was.txt': 'the old integration\n' },
      [place.base],
      'the old integration',
    );
    const after = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'upstream\n', 'now.txt': 'the new integration\n' },
      [place.base],
      'the new integration',
    );
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/integration', oldOid: null, newOid: after.oid, force: true },
    ]);
    const done = await jobIn(place, 'completed', {
      previous_integration_head: before.oid,
      new_integration_head: after.oid,
    });

    const response = await call(place, `/rebuilds/${done}/rollback`, { method: 'POST' });

    expect(response.status).toBe(200);
    expect(((await response.json()) as { rolled_back_to: string }).rolled_back_to).toBe(before.oid);
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.find((ref) => ref.name === 'refs/heads/integration')?.oid).toBe(before.oid);
  });

  it('refuses a job with nothing to return to, and one on a busy workspace', async () => {
    const place = await world();
    const first = await jobIn(place, 'completed');

    expect((await call(place, `/rebuilds/${first}/rollback`, { method: 'POST' })).status).toBe(409);

    const done = await jobIn(place, 'completed', { previous_integration_head: place.base });
    const held = await place.service.locks.take(`workspace:${place.slug}`);
    const busy = await call(place, `/rebuilds/${done}/rollback`, { method: 'POST' });
    await held?.release();

    expect(busy.status).toBe(409);
    expect(((await busy.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
  });

  it('answers 500 and leaves the branch where it was when the reset fails', async () => {
    const place = await world();
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/integration', oldOid: null, newOid: place.base, force: true },
    ]);
    const done = await jobIn(place, 'completed', { previous_integration_head: place.base });
    const original = place.repos.updateRefs.bind(place.repos);
    place.repos.updateRefs = (() =>
      Promise.reject(new Error('the store will not move it'))) as typeof place.repos.updateRefs;

    const response = await call(place, `/rebuilds/${done}/rollback`, { method: 'POST' });
    place.repos.updateRefs = original;

    expect(response.status).toBe(500);
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.find((ref) => ref.name === 'refs/heads/integration')?.oid).toBe(place.base);
  });
});
