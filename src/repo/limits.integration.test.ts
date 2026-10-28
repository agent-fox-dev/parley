import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import { objectId, writePack } from '../platform/local/pack.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RelationalStore } from '../platform/index.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import {
  REPOSITORY_BYTE_CAP,
  releaseRepositoryBytes,
  repositoryBytes,
  reserveRepositoryBytes,
} from './limits.js';

const AT = SEED_NOW.toISOString();
const REPO = 'a-repository';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function store(): Promise<RelationalStore> {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  return seeded.store;
}

async function refused(work: () => Promise<unknown>): Promise<HttpError> {
  try {
    await work();
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the write was accepted when it should have been refused');
}

describe('the bytes a repository is accounted as holding', () => {
  it('starts at nothing and adds up what is written', async () => {
    const relational = await store();

    expect(await repositoryBytes(relational, REPO)).toBe(0);

    await reserveRepositoryBytes(relational, REPO, 1000, AT);
    await reserveRepositoryBytes(relational, REPO, 500, AT);

    expect(await repositoryBytes(relational, REPO)).toBe(1500);
  });

  it('is counted per repository', async () => {
    const relational = await store();

    await reserveRepositoryBytes(relational, REPO, 1000, AT);

    expect(await repositoryBytes(relational, 'another')).toBe(0);
  });

  it('is forgotten when the repository is removed', async () => {
    const relational = await store();
    await reserveRepositoryBytes(relational, REPO, 1000, AT);

    await releaseRepositoryBytes(relational, REPO);

    expect(await repositoryBytes(relational, REPO)).toBe(0);
  });
});

describe('a write that would pass the cap', () => {
  it('is refused with the limit named, and nothing is counted against it', async () => {
    const relational = await store();
    const cap = 1000;
    await reserveRepositoryBytes(relational, REPO, 900, AT, cap);

    const err = await refused(() => reserveRepositoryBytes(relational, REPO, 200, AT, cap));

    expect(err.status).toBe(507);
    expect(err.message).toContain('MB');
    expect(await repositoryBytes(relational, REPO)).toBe(900);
  });

  it('leaves what the repository already holds readable and unchanged', async () => {
    const relational = await store();
    const dir = mkdtempSync(join(tmpdir(), 'parley-limits-'));
    cleanups.push(() => {
      rmSync(dir, { recursive: true, force: true });
    });
    const repos = localRepositoryStore(dir);
    await repos.create(REPO);

    const data = new TextEncoder().encode('an object that was written');
    const oid = objectId('blob', data);
    await repos.receivePack(REPO, writePack([{ oid, type: 'blob', data }]), [
      { name: 'refs/heads/main', oldOid: null, newOid: oid },
    ]);
    const cap = 100;
    await reserveRepositoryBytes(relational, REPO, 90, AT, cap);

    await refused(() => reserveRepositoryBytes(relational, REPO, 50, AT, cap));

    expect(await repos.listRefs(REPO)).toEqual([{ name: 'refs/heads/main', oid }]);
    const object = await repos.readObject(REPO, oid);
    expect(new TextDecoder().decode(object?.data ?? new Uint8Array())).toBe(
      'an object that was written',
    );
  });

  it('accepts a write that lands exactly on the cap', async () => {
    const relational = await store();
    const cap = 1000;

    await reserveRepositoryBytes(relational, REPO, 1000, AT, cap);

    expect(await repositoryBytes(relational, REPO)).toBe(1000);
  });

  it('names a gigabyte when the cap is the deployment default', async () => {
    const relational = await store();

    const err = await refused(() =>
      reserveRepositoryBytes(relational, REPO, REPOSITORY_BYTE_CAP + 1, AT),
    );

    expect(err.message).toContain('1024 MB');
  });
});

/**
 * The sizes the package claims. Building them takes minutes and
 * gigabytes, so they run only when asked for; the case says what is
 * missing rather than passing on a fixture nobody built.
 */
const LARGE = process.env['PARLEY_LARGE_FIXTURES'] === '1';

describe.skipIf(!LARGE)(
  `a repository at the stated sizes${LARGE ? '' : ' (needs PARLEY_LARGE_FIXTURES=1)'}`,
  () => {
    it('takes ten thousand refs, reads them back and accepts another write', async () => {
      const dir = mkdtempSync(join(tmpdir(), 'parley-large-'));
      cleanups.push(() => {
        rmSync(dir, { recursive: true, force: true });
      });
      const repos = localRepositoryStore(dir);
      await repos.create(REPO);

      const data = new TextEncoder().encode('shared content');
      const oid = objectId('blob', data);
      await repos.receivePack(REPO, writePack([{ oid, type: 'blob', data }]), [
        { name: 'refs/heads/base', oldOid: null, newOid: oid },
      ]);
      await repos.updateRefs(
        REPO,
        Array.from({ length: 10_000 }, (_, i) => ({
          name: `refs/heads/branch-${String(i)}`,
          oldOid: null,
          newOid: oid,
        })),
      );

      expect(await repos.listRefs(REPO)).toHaveLength(10_001);

      await repos.updateRefs(REPO, [{ name: 'refs/heads/one-more', oldOid: null, newOid: oid }]);
      expect(await repos.listRefs(REPO)).toHaveLength(10_002);
    }, 300_000);
  },
);
