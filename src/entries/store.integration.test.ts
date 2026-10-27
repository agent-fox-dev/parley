import { afterEach, describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import { deploymentKey, openWith, sealWith } from '../secrets/store.js';
import { SECRET_KEY } from '../testutil/app.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import {
  type Cipher,
  createEntries,
  deleteEntry,
  listEntries,
  plainCipher,
  replaceEntry,
  userScope,
} from './store.js';
import { MAX_ENTRIES_PER_SCOPE } from './validate.js';

const SCOPE = userScope('a-user');
const AT = SEED_NOW.toISOString();

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

async function world(): Promise<{ store: RelationalStore; sealing: Cipher }> {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  const key = (await deploymentKey(SECRET_KEY)) as CryptoKey;
  return { store: seeded.store, sealing: { seal: sealWith(key), open: openWith(key) } };
}

async function caught(work: () => Promise<unknown>): Promise<HttpError> {
  try {
    await work();
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the write was accepted when it should have been refused');
}

describe('an entry in the store', () => {
  it('is found however the key that looks for it is cased', async () => {
    const { store } = await world();
    await createEntries(store, SCOPE, 'variable', [{ key: 'My_Key', value: 'v' }], AT, plainCipher);

    const replaced = await replaceEntry(
      store,
      SCOPE,
      'variable',
      'MY_KEY',
      'changed',
      AT,
      plainCipher,
    );

    expect(replaced.value).toBe('changed');
    // The casing it was created with survives the lookup.
    expect(replaced.key).toBe('My_Key');

    await deleteEntry(store, SCOPE, 'variable', 'my_key');
    expect(await listEntries(store, SCOPE, 'variable', plainCipher)).toEqual([]);
  });

  it('is one entry however the key that creates it again is cased', async () => {
    const { store } = await world();
    await createEntries(store, SCOPE, 'variable', [{ key: 'my_key', value: 'v' }], AT, plainCipher);

    const err = await caught(() =>
      createEntries(store, SCOPE, 'variable', [{ key: 'MY_KEY', value: 'w' }], AT, plainCipher),
    );

    expect(err.status).toBe(409);
    expect(await listEntries(store, SCOPE, 'variable', plainCipher)).toHaveLength(1);
  });

  it('does not collide with the same key of another kind or another scope', async () => {
    const { store, sealing } = await world();

    await createEntries(store, SCOPE, 'variable', [{ key: 'SHARED', value: 'v' }], AT, plainCipher);
    await createEntries(store, SCOPE, 'secret', [{ key: 'SHARED', value: 's' }], AT, sealing);
    await createEntries(
      store,
      userScope('other'),
      'variable',
      [{ key: 'SHARED', value: 'o' }],
      AT,
      plainCipher,
    );

    expect(await listEntries(store, SCOPE, 'variable', plainCipher)).toHaveLength(1);
    expect(await listEntries(store, SCOPE, 'secret', sealing)).toHaveLength(1);
  });
});

describe('a list of entries', () => {
  it.each(['secret', 'variable'] as const)('is ordered by key ignoring case: %s', async (kind) => {
    const { store, sealing } = await world();
    const cipher = kind === 'secret' ? sealing : plainCipher;
    await createEntries(
      store,
      SCOPE,
      kind,
      [
        { key: 'C_KEY', value: 'c' },
        { key: 'a_key', value: 'a' },
        { key: 'B_KEY', value: 'b' },
        { key: 'A_OTHER', value: 'o' },
      ],
      AT,
      cipher,
    );

    const listed = await listEntries(store, SCOPE, kind, cipher);

    expect(listed.map((e) => e.key)).toEqual(['a_key', 'A_OTHER', 'B_KEY', 'C_KEY']);
  });
});

async function filled(count: number) {
  const { store, sealing } = await world();
  await createEntries(
    store,
    SCOPE,
    'variable',
    Array.from({ length: count }, (_, i) => ({ key: `K_${String(i)}`, value: 'v' })),
    AT,
    plainCipher,
  );
  return { store, sealing };
}

describe('the hundred entries a scope may hold', () => {
  it('accepts a create that brings the scope to exactly the limit', async () => {
    const { store } = await filled(MAX_ENTRIES_PER_SCOPE - 2);

    await createEntries(
      store,
      SCOPE,
      'variable',
      [
        { key: 'LAST_A', value: 'v' },
        { key: 'LAST_B', value: 'v' },
      ],
      AT,
      plainCipher,
    );

    expect(await listEntries(store, SCOPE, 'variable', plainCipher)).toHaveLength(
      MAX_ENTRIES_PER_SCOPE,
    );
  });

  it('refuses the next entry once the scope is full', async () => {
    const { store } = await filled(MAX_ENTRIES_PER_SCOPE);

    const err = await caught(() =>
      createEntries(
        store,
        SCOPE,
        'variable',
        [{ key: 'ONE_TOO_MANY', value: 'v' }],
        AT,
        plainCipher,
      ),
    );

    expect(err.status).toBe(400);
  });

  it('writes none of a create that would take the scope past the limit', async () => {
    const { store } = await filled(MAX_ENTRIES_PER_SCOPE - 2);

    const err = await caught(() =>
      createEntries(
        store,
        SCOPE,
        'variable',
        [
          { key: 'ONE', value: 'v' },
          { key: 'TWO', value: 'v' },
          { key: 'THREE', value: 'v' },
        ],
        AT,
        plainCipher,
      ),
    );

    expect(err.status).toBe(400);
    expect(await listEntries(store, SCOPE, 'variable', plainCipher)).toHaveLength(
      MAX_ENTRIES_PER_SCOPE - 2,
    );
  });

  it('counts each kind on its own', async () => {
    const { store, sealing } = await filled(MAX_ENTRIES_PER_SCOPE);

    await createEntries(store, SCOPE, 'secret', [{ key: 'A_SECRET', value: 's' }], AT, sealing);

    expect(await listEntries(store, SCOPE, 'secret', sealing)).toHaveLength(1);
  });
});

describe('a create that names a key the scope already holds', () => {
  it('writes none of its entries', async () => {
    const { store } = await world();
    await createEntries(
      store,
      SCOPE,
      'variable',
      [{ key: 'EXISTING', value: 'first' }],
      AT,
      plainCipher,
    );

    const err = await caught(() =>
      createEntries(
        store,
        SCOPE,
        'variable',
        [
          { key: 'FRESH', value: 'v' },
          { key: 'existing', value: 'second' },
        ],
        '2026-04-01T00:00:00.000Z',
        plainCipher,
      ),
    );

    const listed = await listEntries(store, SCOPE, 'variable', plainCipher);
    expect(err.status).toBe(409);
    expect(listed.map((e) => e.key)).toEqual(['EXISTING']);
    expect(listed[0]?.value).toBe('first');
    expect(listed[0]?.updated_at).toBe(AT);
  });
});

describe('a secret in the entry store', () => {
  it('is sealed in the store and comes back as it went in', async () => {
    const { store, sealing } = await world();

    await createEntries(
      store,
      SCOPE,
      'secret',
      [{ key: 'A_SECRET', value: 'SENTINEL-9f3a' }],
      AT,
      sealing,
    );

    const raw = await store.all("SELECT value FROM entries WHERE kind = 'secret'");
    expect(JSON.stringify(raw)).not.toContain('SENTINEL-9f3a');

    const listed = await listEntries(store, SCOPE, 'secret', sealing);
    expect(listed[0]?.value).toBe('SENTINEL-9f3a');
  });

  it('is stored in clear when it is a variable, so a reader needs no key', async () => {
    const { store } = await world();

    await createEntries(
      store,
      SCOPE,
      'variable',
      [{ key: 'A_VAR', value: 'plain' }],
      AT,
      plainCipher,
    );

    const raw = await store.all<{ value: string }>(
      "SELECT value FROM entries WHERE kind = 'variable'",
    );
    expect(raw[0]?.value).toBe('plain');
  });
});

describe('replacing and deleting what is not there', () => {
  it.each([
    [
      'a replace',
      async (store: RelationalStore) =>
        replaceEntry(store, SCOPE, 'variable', 'ABSENT', 'v', AT, plainCipher),
    ],
    ['a delete', async (store: RelationalStore) => deleteEntry(store, SCOPE, 'variable', 'ABSENT')],
  ])('answers 404 for %s', async (_name, work) => {
    const { store } = await world();

    expect((await caught(async () => work(store))).status).toBe(404);
  });
});
