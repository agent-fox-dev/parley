import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { objectId, writePack } from './local/pack.js';
import { localRepositoryStore } from './local/repostore.js';
import { artifactsRepositoryStore } from './cloudflare/artifacts.js';
import { fakeArtifacts } from '../testutil/fakeartifacts.js';
import type { RepositoryStore } from './repostore.js';

/**
 * One suite, run against every implementation a deployment can be wired
 * with. A case that passes against one and fails against the other is a
 * defect in the interface, so the suite is the same code for both.
 */
interface Candidate {
  name: string;
  make: () => RepositoryStore;
}

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function tempRoot(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-repostore-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

const candidates: Candidate[] = [
  { name: 'the local store over a bare repository', make: () => localRepositoryStore(tempRoot()) },
  {
    name: 'the Cloudflare store over Artifacts',
    make: () => {
      const fake = fakeArtifacts();
      cleanups.push(fake.dispose);
      return artifactsRepositoryStore(fake.binding, { fetch: fake.fetch });
    },
  },
];

const bytes = (value: string) => new TextEncoder().encode(value);
const blob = (value: string) => ({
  oid: objectId('blob', bytes(value)),
  type: 'blob' as const,
  data: bytes(value),
});

describe.each(candidates)('the repository store contract: $name', ({ make }) => {
  async function withRepo(): Promise<{ store: RepositoryStore; name: string }> {
    const store = make();
    const name = 'a-workspace';
    await store.create(name);
    return { store, name };
  }

  it('says a repository it created exists, and one it did not does not', async () => {
    const { store, name } = await withRepo();

    expect(await store.exists(name)).toBe(true);
    expect(await store.exists('never-created')).toBe(false);
  });

  it('creating the same repository twice is not an error', async () => {
    const { store, name } = await withRepo();

    await expect(store.create(name)).resolves.toBeUndefined();
    expect(await store.exists(name)).toBe(true);
  });

  it('a repository it removed is gone, and removing what is absent is not an error', async () => {
    const { store, name } = await withRepo();

    await store.remove(name);
    await expect(store.remove(name)).resolves.toBeUndefined();

    expect(await store.exists(name)).toBe(false);
  });

  it('holds no refs when it is new', async () => {
    const { store, name } = await withRepo();

    expect(await store.listRefs(name)).toEqual([]);
  });

  it('takes a pack and the refs it makes reachable, and lists them back', async () => {
    const { store, name } = await withRepo();
    const one = blob('the first object');
    const two = blob('the second object');

    await store.receivePack(name, writePack([one, two]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
      { name: 'refs/heads/other', oldOid: null, newOid: two.oid },
    ]);

    expect(await store.listRefs(name)).toEqual([
      { name: 'refs/heads/main', oid: one.oid },
      { name: 'refs/heads/other', oid: two.oid },
    ]);
  });

  it('serves back a pack holding what it was given', async () => {
    const { store, name } = await withRepo();
    const one = blob('something to serve');
    await store.receivePack(name, writePack([one]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);

    const served = await store.servePack(name, [one.oid]);

    expect(new TextDecoder().decode(served.subarray(0, 4))).toBe('PACK');
    expect(served.length).toBeGreaterThan(12);
  });

  it('reads back an object it was given', async () => {
    const { store, name } = await withRepo();
    const one = blob('read me back');
    await store.receivePack(name, writePack([one]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);

    const object = await store.readObject(name, one.oid);

    expect(new TextDecoder().decode(object?.data ?? new Uint8Array())).toBe('read me back');
  });

  it('answers with nothing for an object it does not hold', async () => {
    const { store, name } = await withRepo();

    expect(await store.readObject(name, '0'.repeat(40))).toBeUndefined();
  });

  it('moves a ref it already holds', async () => {
    const { store, name } = await withRepo();
    const one = blob('first');
    const two = blob('second');
    await store.receivePack(name, writePack([one, two]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);

    await store.updateRefs(name, [{ name: 'refs/heads/main', oldOid: one.oid, newOid: two.oid }]);

    expect(await store.listRefs(name)).toEqual([{ name: 'refs/heads/main', oid: two.oid }]);
  });

  it('deletes a ref', async () => {
    const { store, name } = await withRepo();
    const one = blob('doomed');
    await store.receivePack(name, writePack([one]), [
      { name: 'refs/heads/doomed', oldOid: null, newOid: one.oid },
    ]);

    await store.updateRefs(name, [{ name: 'refs/heads/doomed', oldOid: one.oid, newOid: null }]);

    expect(await store.listRefs(name)).toEqual([]);
  });

  it('takes a ref outside refs/heads/ and refs/tags/', async () => {
    const { store, name } = await withRepo();
    const one = blob('replaced');
    await store.receivePack(name, writePack([one]), [
      { name: 'refs/hub/replaced/a-branch', oldOid: null, newOid: one.oid },
    ]);

    expect(await store.listRefs(name)).toEqual([
      { name: 'refs/hub/replaced/a-branch', oid: one.oid },
    ]);
  });

  it('moves a ref backwards when the caller forces it', async () => {
    const { store, name } = await withRepo();
    const one = blob('before');
    const two = blob('after');
    await store.receivePack(name, writePack([one, two]), [
      { name: 'refs/heads/main', oldOid: null, newOid: two.oid },
    ]);

    await store.updateRefs(name, [
      { name: 'refs/heads/main', oldOid: two.oid, newOid: one.oid, force: true },
    ]);

    expect(await store.listRefs(name)).toEqual([{ name: 'refs/heads/main', oid: one.oid }]);
  });

  it('reports the same three capabilities it behaves by', async () => {
    const { store } = await withRepo();

    expect(store.capabilities).toEqual({
      arbitraryRefNamespaces: true,
      nonFastForward: true,
      refDeletion: true,
    });
  });
});

describe('a store that lacks a capability', () => {
  const one = blob('untouched');

  async function withBaseline(missing: keyof RepositoryStore['capabilities']) {
    const root = tempRoot();
    const full = localRepositoryStore(root);
    await full.create('a-workspace');
    await full.receivePack('a-workspace', writePack([one]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);

    const limited = localRepositoryStore(root, {
      arbitraryRefNamespaces: true,
      nonFastForward: true,
      refDeletion: true,
      [missing]: false,
    });
    return { limited, before: await full.listRefs('a-workspace') };
  }

  it.each([
    [
      'refs outside the two namespaces',
      'arbitraryRefNamespaces' as const,
      { name: 'refs/hub/replaced/x', oldOid: null, newOid: one.oid },
      'refs outside',
    ],
    [
      'a rewind',
      'nonFastForward' as const,
      { name: 'refs/heads/main', oldOid: 'f'.repeat(40), newOid: one.oid, force: true },
      'non-fast-forward',
    ],
    [
      'a deletion',
      'refDeletion' as const,
      { name: 'refs/heads/main', oldOid: one.oid, newOid: null },
      'ref deletion',
    ],
  ])('refuses %s, naming it, and changes nothing', async (_name, missing, change, says) => {
    const { limited, before } = await withBaseline(missing);

    const refused = await limited
      .updateRefs('a-workspace', [change])
      .then(() => undefined)
      .catch((err: unknown) => err as { status?: number; message?: string });

    expect(refused?.status).toBe(501);
    expect(refused?.message).toContain(says);
    expect(await limited.listRefs('a-workspace')).toEqual(before);
  });

  it('applies no substitute effect in place of what it refused', async () => {
    const { limited, before } = await withBaseline('refDeletion');

    await limited
      .updateRefs('a-workspace', [{ name: 'refs/heads/main', oldOid: one.oid, newOid: null }])
      .catch(() => undefined);

    // Not emptied, not pointed at the zero oid, not renamed: unchanged.
    expect(await limited.listRefs('a-workspace')).toEqual(before);
  });
});

describe('the token the Cloudflare store mints for itself', () => {
  it('is used but never handed out', async () => {
    const fake = fakeArtifacts();
    cleanups.push(fake.dispose);
    const records: string[] = [];
    const store = artifactsRepositoryStore(fake.binding, {
      fetch: (async (input: string | URL | Request, init?: RequestInit) => {
        records.push(`${String(input)} ${JSON.stringify(init?.headers ?? {})}`);
        return fake.fetch(input as string, init);
      }) as unknown as typeof fetch,
    });

    await store.create('a-workspace');
    const one = blob('something');
    await store.receivePack('a-workspace', writePack([one]), [
      { name: 'refs/heads/main', oldOid: null, newOid: one.oid },
    ]);
    const refs = await store.listRefs('a-workspace');
    const served = await store.servePack('a-workspace', [one.oid]);
    const failed = await store
      .updateRefs('a-workspace', [
        { name: 'refs/heads/main', oldOid: 'f'.repeat(40), newOid: one.oid },
      ])
      .then(() => 'no failure')
      .catch((err: Error) => err.message);

    const token = fake.lastToken();
    expect(token).not.toBe('');
    // It reached the remote, so it was used and not merely withheld.
    expect(records.join('\n')).toContain(token);

    // And it is in nothing the service would hand on.
    expect(JSON.stringify(refs)).not.toContain(token);
    expect(new TextDecoder().decode(served)).not.toContain(token);
    expect(failed).not.toContain(token);
  });
});
