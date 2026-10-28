import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { objectId, writePack } from '../platform/local/pack.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { pushOutOnArchive } from './retire.js';
import { MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { ensureRepository, repositoryOf } from './repository.js';

const TOKEN = 'ghp_SENTINEL_retire_9f3a';
const REPO = 'a-repository';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const bytes = (value: string) => new TextEncoder().encode(value);
const blob = (value: string) => ({
  oid: objectId('blob', bytes(value)),
  type: 'blob' as const,
  data: bytes(value),
});

async function world(options: Parameters<typeof fakeRemote>[0] = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'parley-retire-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos: RepositoryStore = localRepositoryStore(dir);
  await repos.create(REPO);
  const remote = fakeRemote({ requires: `Bearer ${TOKEN}`, ...options });
  cleanups.push(remote.dispose);
  return { repos, remote };
}

const credential = { kind: 'token' as const, token: TOKEN };

describe('archiving a workspace that can be pushed out', () => {
  it('puts every branch on origin and then lets the repository go', async () => {
    const { repos, remote } = await world();
    const shared = blob('on both');
    const only = blob('only here');
    await remote.put('refs/heads/main', 'on both');
    await repos.receivePack(REPO, writePack([shared, only]), [
      { name: 'refs/heads/main', oldOid: null, newOid: shared.oid },
      { name: 'refs/heads/local-only', oldOid: null, newOid: only.oid },
    ]);

    const outcome = await pushOutOnArchive(repos, {
      repository: REPO,
      gitUrl: remote.url,
      credential,
      fetch: remote.fetch,
    });

    expect(outcome).toBe('released');
    expect((await remote.refs()).map((ref) => ref.name)).toContain('refs/heads/local-only');
    expect(await repos.exists(REPO)).toBe(false);
  });

  it('rewrites nothing that origin already holds', async () => {
    const { repos, remote } = await world();
    const theirs = await remote.put('refs/heads/main', 'origin content');
    const mine = blob('only here');
    await repos.receivePack(REPO, writePack([mine]), [
      { name: 'refs/heads/local-only', oldOid: null, newOid: mine.oid },
    ]);

    await pushOutOnArchive(repos, {
      repository: REPO,
      gitUrl: remote.url,
      credential,
      fetch: remote.fetch,
    });

    const after = await remote.refs();
    expect(after.find((ref) => ref.name === 'refs/heads/main')?.oid).toBe(theirs);
  });

  it('lets the repository go when there is nothing to push', async () => {
    const { repos, remote } = await world();

    const outcome = await pushOutOnArchive(repos, {
      repository: REPO,
      gitUrl: remote.url,
      credential,
      fetch: remote.fetch,
    });

    expect(outcome).toBe('released');
  });
});

async function keptBecause(options: Parameters<typeof fakeRemote>[0], withCredential = true) {
  const { repos, remote } = await world(options);
  const only = blob('only here');
  await repos.receivePack(REPO, writePack([only]), [
    { name: 'refs/heads/local-only', oldOid: null, newOid: only.oid },
  ]);

  const outcome = await pushOutOnArchive(repos, {
    repository: REPO,
    gitUrl: remote.url,
    ...(withCredential ? { credential } : {}),
    fetch: remote.fetch,
  });
  return { outcome, repos };
}

describe('archiving a workspace that cannot be pushed out', () => {
  it.each([
    ['origin refuses the branch', { refusesPush: 'the hook said no' }, true],
    ['the branch has diverged from origin', { refusesPush: 'non-fast-forward' }, true],
    ['the remote cannot be reached', { unreachable: true }, true],
    ['the workspace has no usable credential', {}, false],
  ])('keeps the repository when %s', async (_name, options, withCredential) => {
    const { outcome, repos } = await keptBecause(options, withCredential);

    expect(outcome).toBe('kept');
    expect(await repos.exists(REPO)).toBe(true);
    expect((await repos.listRefs(REPO)).map((ref) => ref.name)).toContain('refs/heads/local-only');
  });
});

describe('deleting a workspace', () => {
  it('takes its repository with it', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'parley-delete-'));
    cleanups.push(() => {
      rmSync(dir, { recursive: true, force: true });
    });
    const repos = localRepositoryStore(dir);
    const service = await identityService([], { repos });
    cleanups.push(service.dispose);

    const owner = await seedUser(service.store);
    const org = await seedOrg(service.store, owner);
    const workspace = await seedWorkspace(service.store, {
      slug: 'doomed',
      owner_id: owner.id,
      org_id: org.id,
      status: 'archived',
    });
    const key = (await seedApiKey(service.store, owner)).presented;
    await ensureRepository(repos, workspace);
    expect(await repos.exists(repositoryOf(workspace))).toBe(true);

    const response = await service.app.request(
      `${MOUNT}/workspaces/doomed`,
      as(key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    expect(await repos.exists(repositoryOf(workspace))).toBe(false);
  });
});
