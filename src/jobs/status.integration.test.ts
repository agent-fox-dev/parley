import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { storeJobs } from '../platform/store_jobs.js';
import { findWorkspace, updateWorkspace } from '../store/workspaces.js';
import { repositoryOf } from '../repo/repository.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { SEED_NOW, freshStore, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { deploymentKey, sealedSecrets } from '../secrets/store.js';
import { SECRET_KEY } from '../testutil/app.js';
import { CLONING_DEADLINE_MS, expireStalledClones, runCopyIn } from './copy_in.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function world(remoteOptions: Parameters<typeof fakeRemote>[0] = {}) {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-status-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const remote = fakeRemote(remoteOptions);
  cleanups.push(remote.dispose);

  const moving = movableClock(SEED_NOW);
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
    moving,
    store: seeded.store,
    workspace,
    repos,
    deps: {
      store: seeded.store,
      repos,
      secrets: sealedSecrets(seeded.store, key),
      jobs: storeJobs(seeded.store, moving.clock),
      clock: moving.clock,
      fetch: remote.fetch,
    },
  };
}

/** Every status the workspace passed through, in order. */
async function watched(
  held: Awaited<ReturnType<typeof world>>,
  run: () => Promise<unknown>,
): Promise<string[]> {
  const seen: string[] = [(await findWorkspace(held.store, 'a-project'))?.status ?? ''];
  await run();
  const after = (await findWorkspace(held.store, 'a-project'))?.status ?? '';
  if (after !== seen.at(-1)) {
    seen.push(after);
  }
  return seen;
}

describe('the status a copy leaves behind', () => {
  it('goes pending, then ready, and records the head commit', async () => {
    const held = await world();
    const oid = await held.remote.put('refs/heads/main', 'content');

    const seen = await watched(held, async () => runCopyIn(held.deps, { slug: 'a-project' }));

    expect(seen).toEqual(['pending', 'ready']);
    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.head_sha).toBe(oid);
    expect(after?.head_sha).toMatch(/^[0-9a-f]{40}$/);
    expect(after?.clone_error).toBeNull();
  });

  it('goes pending, then failed, and records no head commit', async () => {
    const held = await world({ fails: 500 });
    await held.remote.put('refs/heads/main', 'content');

    const seen = await watched(held, async () => runCopyIn(held.deps, { slug: 'a-project' }));

    expect(seen).toEqual(['pending', 'failed']);
    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.head_sha).toBeNull();
    expect(after?.clone_error).not.toBeNull();
  });

  it('never goes backwards once it is ready', async () => {
    const held = await world();
    await held.remote.put('refs/heads/main', 'content');
    await runCopyIn(held.deps, { slug: 'a-project' });

    // The same job taken up again finds nothing to bring.
    await runCopyIn(held.deps, { slug: 'a-project' });

    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('ready');
  });
});

describe('a workspace left in cloning', () => {
  it('is given up on after the deadline, with the reason recorded', async () => {
    const held = await world();
    await updateWorkspace(
      held.store,
      'a-project',
      { status: 'cloning' },
      held.moving.clock.now().toISOString(),
    );

    expect(await expireStalledClones(held.store, held.moving.clock)).toBe(0);
    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('cloning');

    held.moving.advance(CLONING_DEADLINE_MS + 1000);

    expect(await expireStalledClones(held.store, held.moving.clock)).toBe(1);
    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('failed');
    expect(after?.clone_error).toContain('timed out');
  });

  it('can be archived once it has been given up on', async () => {
    const held = await world();
    await updateWorkspace(
      held.store,
      'a-project',
      { status: 'cloning' },
      held.moving.clock.now().toISOString(),
    );
    held.moving.advance(CLONING_DEADLINE_MS + 1000);
    await expireStalledClones(held.store, held.moving.clock);

    // Archiving refuses a cloning workspace and takes a failed one.
    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).not.toBe('cloning');
  });

  it('leaves a workspace that is not cloning alone', async () => {
    const held = await world();
    held.moving.advance(CLONING_DEADLINE_MS * 10);

    expect(await expireStalledClones(held.store, held.moving.clock)).toBe(0);
    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('pending');
  });
});

describe('a workspace whose repository was kept', () => {
  it('reuses it on reactivation instead of copying the remote again', async () => {
    const held = await world();
    await held.remote.put('refs/heads/main', 'content');
    await runCopyIn(held.deps, { slug: 'a-project' });

    // A branch that exists only here, as a kept repository would hold.
    const name = repositoryOf(held.workspace);
    const refs = await held.repos.listRefs(name);
    const oid = refs[0]?.oid ?? '';
    await held.repos.updateRefs(name, [
      { name: 'refs/heads/local-only', oldOid: null, newOid: oid },
    ]);

    const before = held.remote.requests();
    await updateWorkspace(
      held.store,
      'a-project',
      { status: 'pending' },
      held.moving.clock.now().toISOString(),
    );
    await runCopyIn(held.deps, { slug: 'a-project' });

    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('ready');
    expect((await held.repos.listRefs(name)).map((ref) => ref.name)).toContain(
      'refs/heads/local-only',
    );
    // It asked the remote what it holds, and took nothing across.
    expect(held.remote.requests() - before).toBeLessThanOrEqual(2);
  });
});
