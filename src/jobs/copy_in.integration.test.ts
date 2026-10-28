import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { storeJobs } from '../platform/store_jobs.js';
import { workspaceScope } from '../platform/secrets.js';
import { findWorkspace } from '../store/workspaces.js';
import { repositoryOf } from '../repo/repository.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { SEED_NOW, freshStore, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { deploymentKey, openWith, sealWith } from '../secrets/store.js';
import { sealedSecrets } from '../secrets/store.js';
import { SECRET_KEY } from '../testutil/app.js';
import { type CopyInPayload, runCopyIn } from './copy_in.js';

const TOKEN = 'ghp_SENTINEL_copy_9f3a';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function world(remoteOptions: Parameters<typeof fakeRemote>[0] = {}) {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-copy-job-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const remote = fakeRemote(remoteOptions);
  cleanups.push(remote.dispose);

  const key = (await deploymentKey(SECRET_KEY)) as CryptoKey;
  const clock = movableClock(SEED_NOW).clock;
  const owner = await seedUser(seeded.store);
  await seedOrg(seeded.store, owner);
  const workspace = await seedWorkspace(seeded.store, {
    slug: 'a-project',
    owner_id: owner.id,
    git_url: remote.url,
    status: 'pending',
  });

  void sealWith;
  void openWith;
  return {
    remote,
    store: seeded.store,
    workspace,
    deps: {
      store: seeded.store,
      repos: localRepositoryStore(dir),
      secrets: sealedSecrets(seeded.store, key),
      jobs: storeJobs(seeded.store, clock),
      clock,
      fetch: remote.fetch,
    },
  };
}

async function drain(
  deps: Awaited<ReturnType<typeof world>>['deps'],
  payload: CopyInPayload,
): Promise<number> {
  let units = 0;
  let outcome: 'done' | 'more' = 'more';
  while (outcome === 'more' && units < 20) {
    outcome = await runCopyIn(deps, payload);
    units += 1;
  }
  return units;
}

describe('the copy job', () => {
  it('moves the workspace from pending through cloning to ready', async () => {
    const held = await world();
    await held.remote.put('refs/heads/main', 'content');
    await held.deps.repos.create(repositoryOf(held.workspace));

    expect((await findWorkspace(held.store, 'a-project'))?.status).toBe('pending');

    await drain(held.deps, { slug: 'a-project' });

    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('ready');
    expect(after?.clone_error).toBeNull();
    expect(await held.deps.repos.listRefs(repositoryOf(held.workspace))).toHaveLength(1);
  });

  it('uses the credential the workspace stored, and never writes it anywhere', async () => {
    const held = await world({ requires: `Bearer ${TOKEN}` });
    await held.remote.put('refs/heads/main', 'content');
    await held.deps.repos.create(repositoryOf(held.workspace));
    await held.deps.secrets.put(workspaceScope('a-project'), 'GIT_PAT', TOKEN);

    await drain(held.deps, { slug: 'a-project' });

    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('ready');
    expect(held.remote.seen()).toContain(`Bearer ${TOKEN}`);
    expect(JSON.stringify(after)).not.toContain(TOKEN);
  });

  it('leaves a readable failed workspace when the copy cannot finish', async () => {
    const held = await world({ requires: `Bearer ${TOKEN}` });
    await held.remote.put('refs/heads/main', 'content');
    await held.deps.repos.create(repositoryOf(held.workspace));

    // No credential stored, so the remote refuses.
    await drain(held.deps, { slug: 'a-project' });

    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('failed');
    expect(after?.clone_error).not.toBeNull();
    expect(after?.clone_error).not.toContain(TOKEN);
    // Still a workspace: readable, and its other fields intact.
    expect(after?.slug).toBe('a-project');
    expect(after?.git_url).toBe(held.remote.url);
  });

  it('records no credential in what it says went wrong', async () => {
    const held = await world({ fails: 500 });
    await held.remote.put('refs/heads/main', 'content');
    await held.deps.repos.create(repositoryOf(held.workspace));
    await held.deps.secrets.put(workspaceScope('a-project'), 'GIT_PAT', TOKEN);

    await drain(held.deps, { slug: 'a-project' });

    const after = await findWorkspace(held.store, 'a-project');
    expect(after?.status).toBe('failed');
    expect(after?.clone_error ?? '').not.toContain(TOKEN);
  });

  it('finishes quietly when the workspace was deleted while it waited', async () => {
    const held = await world();

    await expect(runCopyIn(held.deps, { slug: 'never-existed' })).resolves.toBe('done');
  });
});
