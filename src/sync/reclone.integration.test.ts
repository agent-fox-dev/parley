import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { storeJobs } from '../platform/store_jobs.js';
import { drainJobs } from '../jobs/drain.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedClock, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Recloning, with a remote that really serves the protocol. The point
 * of the route is that nothing is lost, so every case looks at what
 * origin holds afterwards as well as at what the route answered.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-origin';

interface World {
  service: Service;
  remote: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
  localOnly: string;
}

async function world(
  options: {
    remote?: FakeRemote;
    credential?: boolean;
    /** Narrows the store the service is given, to break one thing. */
    narrow?: (store: RepositoryStore) => RepositoryStore;
  } = {},
): Promise<World> {
  const remote = options.remote ?? fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-reclone-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const onDisk = localRepositoryStore(dir);
  const repos = options.narrow === undefined ? onDisk : options.narrow(onDisk);

  const service = await identityService([], { repos, fetch: remote.fetch, jobs: 'store' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);

  // Origin has one commit; the workspace has that and one of its own.
  const first = await buildCommit(
    remote.store,
    remote.repository,
    { 'a.txt': 'first\n' },
    [],
    'first',
  );
  await remote.store.updateRefs(remote.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid, force: true },
  ]);

  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    branch: 'main',
    git_url: `${remote.url}/origin.git`,
    head_sha: first.oid,
    upstream_head_sha: first.oid,
    sync_error: 'something went wrong last time',
  });

  const repository = repositoryOf(workspace);
  await onDisk.create(repository);
  const here = await buildCommit(onDisk, repository, { 'a.txt': 'first\n' }, [], 'first');
  const own = await buildCommit(
    onDisk,
    repository,
    { 'a.txt': 'first\n', 'only-here.txt': 'made inside parley\n' },
    [here.oid],
    'a commit only parley has',
  );
  await onDisk.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: here.oid, force: true },
    { name: 'refs/heads/only-here', oldOid: null, newOid: own.oid, force: true },
  ]);

  if (options.credential !== false) {
    await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);
  }

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    localOnly: own.oid,
  };
}

async function reclone(place: World): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/reclone`, {
    method: 'POST',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

async function read(place: World): Promise<Record<string, unknown>> {
  const response = await place.service.app.request(`/api/v1/workspaces/${place.slug}`, {
    headers: { authorization: `Bearer ${place.credential}` },
  });
  return (await response.json()) as Record<string, unknown>;
}

describe('a reclone', () => {
  it('pushes out what only it has, discards the repository and copies again', async () => {
    const place = await world();

    const response = await reclone(place);
    const answered = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(answered['status']).toBe('pending');
    expect(answered['sync_error']).toBeNull();
    expect(answered['upstream_head_sha']).toBeNull();

    // The branch that existed only inside Parley is on origin now.
    const onOrigin = await place.remote.store.listRefs(place.remote.repository);
    expect(onOrigin.find((ref) => ref.name === 'refs/heads/only-here')?.oid).toBe(place.localOnly);

    // And the copy runs, taking the workspace to ready.
    // The copy runs a batch at a time, so it is drained to its end.
    for (let round = 0; round < 10; round += 1) {
      await drainJobs({
        store: place.service.store,
        repos: place.repos,
        secrets: place.service.secrets,
        jobs: storeJobs(place.service.store, seedClock()),
        clock: seedClock(),
        fetch: place.remote.fetch,
      });
      if ((await read(place))['status'] !== 'pending') {
        break;
      }
    }
    const settled = await read(place);
    expect(settled['clone_error'], 'the copy recorded no failure').toBeNull();
    expect(settled['status']).toBe('ready');
  });

  it.each([
    ['an origin that refuses the push', { refusesPush: 'origin says no' }, true],
    ['no usable credential', { requires: `Bearer ${TOKEN}` }, false],
  ])('goes on regardless when the push cannot be made: %s', async (_what, options, credential) => {
    const place = await world({ remote: fakeRemote(options), credential });

    const response = await reclone(place);

    expect(response.status).toBe(200);
    expect(((await response.json()) as Record<string, unknown>)['status']).toBe('pending');
    // The old repository really was discarded, whatever the push did:
    // what stands in its place is empty, for the copy to fill.
    expect(await place.repos.listRefs(place.repository)).toEqual([]);
  });
});

describe('a reclone that must not run', () => {
  it.each([
    ['archived', 'archived', 400],
    ['pending', 'pending', 409],
    ['cloning', 'cloning', 409],
  ])(
    'refuses a workspace that is %s, and keeps the repository',
    async (_what, status, expected) => {
      const place = await world();
      await place.service.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
        status,
        place.slug,
      ]);

      const response = await reclone(place);

      expect(response.status).toBe(expected);
      expect(await place.repos.exists(place.repository)).toBe(true);
    },
  );

  it('refuses at once while another operation holds the workspace', async () => {
    const place = await world();
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await reclone(place);

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
    expect(await place.repos.exists(place.repository)).toBe(true);
    await held?.release();
  });

  it('answers 500 and changes nothing when the repository cannot be discarded', async () => {
    const place = await world({
      narrow: (store) => ({
        ...store,
        remove: () => Promise.reject(new Error('the store would not discard it')),
      }),
    });
    const before = await read(place);

    const response = await reclone(place);

    expect(response.status).toBe(500);
    expect((await read(place))['status']).toBe(before['status']);
    expect((await read(place))['sync_error']).toBe(before['sync_error']);
  });

  it('answers 500 and keeps the repository when the workspace cannot be updated', async () => {
    const place = await world();
    const original = place.service.store.run.bind(place.service.store);
    place.service.store.run = (async (sql: string, params?: readonly unknown[]) => {
      if (sql.startsWith('UPDATE workspaces SET status')) {
        throw new Error('the store would not take the update');
      }
      return original(sql, params as never);
    }) as typeof place.service.store.run;

    const response = await reclone(place);

    expect(response.status).toBe(500);
    place.service.store.run = original;
    expect((await read(place))['status']).toBe('ready');
  });
});
