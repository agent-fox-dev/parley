import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import type { RelationalStore } from '../platform/index.js';
import { freshStore } from '../testutil/identity.js';
import { deploymentKey, sealedSecrets } from './store.js';

const KEY_MATERIAL = 'c2VjcmV0LWtleS1vZi10aGlydHktdHdvLWJ5dGVzISE=';
const SENTINEL = 'ghp_SENTINELvalue9f3a';

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

async function secrets(): Promise<{
  store: RelationalStore;
  secrets: Awaited<ReturnType<typeof make>>;
}> {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  return { store: seeded.store, secrets: await make(seeded.store) };
}

async function make(store: RelationalStore) {
  const key = await deploymentKey(KEY_MATERIAL);
  if (key === undefined) {
    throw new Error('the deployment key could not be read');
  }
  return sealedSecrets(store, key);
}

describe('a secret in the store', () => {
  it('comes back as it went in', async () => {
    const { secrets: vault } = await secrets();

    await vault.put(workspaceScope('demo'), 'GIT_PAT', SENTINEL);

    expect(await vault.get(workspaceScope('demo'), 'GIT_PAT')).toBe(SENTINEL);
  });

  it('is nowhere in the store in a form anyone can read', async () => {
    const { store, secrets: vault } = await secrets();

    await vault.put(workspaceScope('demo'), 'GIT_PAT', SENTINEL);
    const rows = await store.all('SELECT * FROM workspace_secrets');

    expect(rows).toHaveLength(1);
    expect(JSON.stringify(rows)).not.toContain(SENTINEL);
  });

  it('seals the same value differently each time, so two equal secrets do not look equal', async () => {
    const { store, secrets: vault } = await secrets();

    await vault.put(workspaceScope('one'), 'GIT_PAT', SENTINEL);
    await vault.put(workspaceScope('two'), 'GIT_PAT', SENTINEL);
    const sealed = (
      await store.all<{ sealed: string }>('SELECT sealed FROM workspace_secrets')
    ).map((row) => row.sealed);

    expect(sealed[0]).not.toBe(sealed[1]);
  });

  it('is replaced rather than duplicated when it is put again', async () => {
    const { store, secrets: vault } = await secrets();

    await vault.put(workspaceScope('demo'), 'GIT_PAT', 'first');
    await vault.put(workspaceScope('demo'), 'GIT_PAT', 'second');

    expect(await vault.get(workspaceScope('demo'), 'GIT_PAT')).toBe('second');
    expect(await store.all('SELECT key FROM workspace_secrets')).toHaveLength(1);
  });

  it('is gone after it is deleted, and deleting what is absent is no error', async () => {
    const { secrets: vault } = await secrets();

    await vault.put(workspaceScope('demo'), 'GIT_PAT', SENTINEL);
    await vault.delete(workspaceScope('demo'), 'GIT_PAT');
    await vault.delete(workspaceScope('demo'), 'NEVER_THERE');

    expect(await vault.get(workspaceScope('demo'), 'GIT_PAT')).toBeUndefined();
  });

  it('is not visible from another scope', async () => {
    const { secrets: vault } = await secrets();

    await vault.put(workspaceScope('one'), 'GIT_PAT', SENTINEL);

    expect(await vault.get(workspaceScope('two'), 'GIT_PAT')).toBeUndefined();
  });

  it('is listed by name alone', async () => {
    const { secrets: vault } = await secrets();

    await vault.put(workspaceScope('demo'), 'GIT_USERNAME', 'user');
    await vault.put(workspaceScope('demo'), 'GIT_PASSWORD', SENTINEL);

    const names = await vault.names(workspaceScope('demo'));

    expect(names.toSorted()).toEqual(['GIT_PASSWORD', 'GIT_USERNAME']);
    expect(JSON.stringify(names)).not.toContain(SENTINEL);
  });
});

describe('the deployment key', () => {
  it('is absent when the deployment configured none', async () => {
    expect(await deploymentKey(undefined)).toBeUndefined();
    expect(await deploymentKey('')).toBeUndefined();
  });

  it('is absent when what the deployment configured is not a 32-byte key', async () => {
    expect(await deploymentKey('too-short')).toBeUndefined();
  });

  it('cannot read a secret sealed under a different key', async () => {
    const seeded = await freshStore();
    disposers.push(seeded.dispose);
    const first = await make(seeded.store);
    await first.put(workspaceScope('demo'), 'GIT_PAT', SENTINEL);

    const other = await deploymentKey('b3RoZXIta2V5LW9mLXRoaXJ0eS10d28tYnl0ZXMhIQ==');
    const second = sealedSecrets(seeded.store, other as CryptoKey);

    await expect(second.get(workspaceScope('demo'), 'GIT_PAT')).rejects.toThrow();
  });
});
