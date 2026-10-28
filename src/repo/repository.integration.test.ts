import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { movableClock } from '../platform/local/clock.js';
import { objectId, writePack } from '../platform/local/pack.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { storeLocks } from '../platform/store_lock.js';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { SEED_NOW, freshStore, seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';
import { ensureRepository, repositoryOf, writingRepository } from './repository.js';

const cleanups: Array<() => void> = [];
let service: Service | undefined;

afterEach(() => {
  service?.dispose();
  service = undefined;
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function tempRoot(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-repo-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

const bytes = (value: string) => new TextEncoder().encode(value);
const blob = (value: string) => ({
  oid: objectId('blob', bytes(value)),
  type: 'blob' as const,
  data: bytes(value),
});

describe('creating a workspace', () => {
  it('gives it a repository in the store, and leaves any other alone', async () => {
    const root = tempRoot();
    const repos = localRepositoryStore(root);
    service = await identityService([], { repos });
    const { app, store } = service;
    const owner = await seedUser(store);
    await seedOrg(store, owner);
    const key = (await seedApiKey(store, owner)).presented;

    const first = await app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', { slug: 'first-one', git_url: 'https://github.com/u/r.git' }, key),
    );
    expect(first.status).toBe(201);
    const firstBody = (await first.json()) as { slug: string; owner_id: string };
    const firstName = repositoryOf(firstBody);

    expect(await repos.exists(firstName)).toBe(true);

    const second = await app.request(
      `${MOUNT}/workspaces`,
      withJson('POST', { slug: 'second-one', git_url: 'https://github.com/u/r.git' }, key),
    );
    const secondBody = (await second.json()) as { slug: string; owner_id: string };

    expect(await repos.exists(repositoryOf(secondBody))).toBe(true);
    expect(await repos.exists(firstName)).toBe(true);
    expect(repositoryOf(secondBody)).not.toBe(firstName);
  });
});

describe('a repository the service already holds', () => {
  it('keeps its refs and its objects when the service is built again', async () => {
    const root = tempRoot();
    const workspace = { slug: 'durable', owner_id: 'owner-1' };
    const one = blob('something worth keeping');

    const before = localRepositoryStore(root);
    const name = await ensureRepository(before, workspace);
    await before.receivePack(name, writePack([one]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);

    // Everything but the store is built again, as a restart would.
    const after = localRepositoryStore(root);

    expect(await after.listRefs(name)).toEqual([{ name: 'refs/heads/main', oid: one.oid }]);
    const object = await after.readObject(name, one.oid);
    expect(new TextDecoder().decode(object?.data ?? new Uint8Array())).toBe(
      'something worth keeping',
    );
  });

  it('is the same repository whether it is asked for twice or once', async () => {
    const repos = localRepositoryStore(tempRoot());
    const workspace = { slug: 'twice', owner_id: 'owner-1' };

    const first = await ensureRepository(repos, workspace);
    const second = await ensureRepository(repos, workspace);

    expect(second).toBe(first);
    expect(await repos.exists(first)).toBe(true);
  });
});

async function locks() {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  return storeLocks(seeded.store, movableClock(SEED_NOW).clock);
}

describe('two writes to one workspace', () => {
  it('take their turns, and the one that may not wait is told the workspace is busy', async () => {
    const held = await locks();
    const workspace = { slug: 'contended' };
    let letGo!: () => void;
    const working = new Promise<void>((resolve) => {
      letGo = resolve;
    });

    const first = writingRepository(held, workspace, async () => {
      await working;
      return 'first';
    });

    const refused = await writingRepository(held, workspace, async () => 'second').catch(
      (err: unknown) => err,
    );

    expect(refused).toBeInstanceOf(HttpError);
    expect((refused as HttpError).status).toBe(409);
    expect((refused as HttpError).errorType).toBe('workspace_busy');

    letGo();
    expect(await first).toBe('first');
    expect(await writingRepository(held, workspace, async () => 'after')).toBe('after');
  });

  it('may overlap when they are writing different workspaces', async () => {
    const held = await locks();
    let letGo!: () => void;
    const working = new Promise<void>((resolve) => {
      letGo = resolve;
    });

    const first = writingRepository(held, { slug: 'one' }, async () => {
      await working;
      return 'one';
    });

    expect(await writingRepository(held, { slug: 'two' }, async () => 'two')).toBe('two');

    letGo();
    await first;
  });
});
