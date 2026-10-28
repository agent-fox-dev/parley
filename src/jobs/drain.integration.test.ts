import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { storeJobs } from '../platform/store_jobs.js';
import { findWorkspace } from '../store/workspaces.js';
import { repositoryOf } from '../repo/repository.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { SEED_NOW, freshStore, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { deploymentKey, sealedSecrets } from '../secrets/store.js';
import { SECRET_KEY } from '../testutil/app.js';
import { queueCopyIn } from './copy_in.js';
import { drainJobs } from './drain.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function world(remoteOptions: Parameters<typeof fakeRemote>[0] = {}) {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-drain-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const remote = fakeRemote(remoteOptions);
  cleanups.push(remote.dispose);

  const clock = movableClock(SEED_NOW).clock;
  const key = (await deploymentKey(SECRET_KEY)) as CryptoKey;
  const owner = await seedUser(seeded.store);
  await seedOrg(seeded.store, owner);
  const workspace = await seedWorkspace(seeded.store, {
    slug: 'a-project',
    owner_id: owner.id,
    git_url: remote.url,
    status: 'pending',
  });
  const repos = localRepositoryStore(dir);
  await repos.create(repositoryOf(workspace));

  return {
    remote,
    store: seeded.store,
    workspace,
    repos,
    deps: {
      store: seeded.store,
      repos,
      secrets: sealedSecrets(seeded.store, key),
      jobs: storeJobs(seeded.store, clock),
      clock,
      fetch: remote.fetch,
    },
  };
}

describe('draining the queue', () => {
  it('runs a queued copy through to ready', async () => {
    const held = await world();
    await held.remote.put('refs/heads/main', 'content');
    await queueCopyIn(held.deps.jobs, 'a-project');

    const ran = await drainJobs(held.deps);

    expect(ran).toBeGreaterThan(0);
    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('ready');
    expect(await held.deps.repos.listRefs(repositoryOf(held.workspace))).toHaveLength(1);
  });

  it('puts a copy back with what it brought, and finishes it on a later drain', async () => {
    const held = await world();
    for (let i = 0; i < 4; i += 1) {
      await held.remote.put(`refs/heads/branch-${String(i)}`, `content ${String(i)}`);
    }
    const id = await queueCopyIn(held.deps.jobs, 'a-project');

    // One job at a time, so a copy that asks to continue goes back.
    let drains = 0;
    while ((await held.deps.jobs.read(id))?.state !== 'done' && drains < 20) {
      await drainJobs(held.deps, { budget: 1 });
      drains += 1;
    }

    expect((await held.deps.jobs.read(id))?.state).toBe('done');
    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('ready');
    expect(await held.deps.repos.listRefs(repositoryOf(held.workspace))).toHaveLength(4);
  });

  it('does nothing when the queue is empty', async () => {
    const held = await world();

    expect(await drainJobs(held.deps)).toBe(0);
  });

  it('gives up on a job nobody can handle, rather than taking it again', async () => {
    const held = await world();
    const id = await held.deps.jobs.enqueue('something-else', {});

    await drainJobs(held.deps);

    expect((await held.deps.jobs.read(id))?.state).toBe('failed');
    expect(await drainJobs(held.deps)).toBe(0);
  });

  it('takes each job once, so two drains do not run one job twice', async () => {
    const held = await world();
    await held.remote.put('refs/heads/main', 'content');
    await queueCopyIn(held.deps.jobs, 'a-project');

    const [first, second] = await Promise.all([
      drainJobs(held.deps, { budget: 1 }),
      drainJobs(held.deps, { budget: 1 }),
    ]);

    expect(first + second).toBe(1);
  });
});
