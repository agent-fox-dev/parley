import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import type { LogSink } from '../http/router.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser } from '../testutil/identity.js';
import { authorizationFor, checkRemoteCredential, copyIn } from './copy_in.js';

const TOKEN = 'ghp_SENTINEL_token_9f3a';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function repos(): RepositoryStore {
  const dir = mkdtempSync(join(tmpdir(), 'parley-copy-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

function remoteHolding(
  refs: Record<string, string>,
  options: Parameters<typeof fakeRemote>[0] = {},
) {
  const remote = fakeRemote(options);
  cleanups.push(remote.dispose);
  return {
    remote,
    seed: async () => {
      const oids: Record<string, string> = {};
      for (const [ref, content] of Object.entries(refs)) {
        oids[ref] = await remote.put(ref, content);
      }
      return oids;
    },
  };
}

function recordingLog(): { log: LogSink; records: string[] } {
  const records: string[] = [];
  const write = (message: string, detail?: unknown) => {
    records.push(`${message} ${detail === undefined ? '' : JSON.stringify(detail)}`);
  };
  return { log: { error: write, info: write }, records };
}

async function runToCompletion(
  store: RepositoryStore,
  request: Parameters<typeof copyIn>[1],
): Promise<{ units: number; progress: Awaited<ReturnType<typeof copyIn>>; asked: string[][] }> {
  let brought: string[] = [];
  let units = 0;
  const asked: string[][] = [];
  let progress = { done: false, brought } as Awaited<ReturnType<typeof copyIn>>;

  while (!progress.done && units < 20) {
    asked.push([...brought]);
    progress = await copyIn(store, { ...request, brought });
    brought = progress.brought;
    units += 1;
  }
  return { units, progress, asked };
}

describe('copying a repository in', () => {
  it('brings every branch and tag at the commit the remote holds', async () => {
    const { remote, seed } = remoteHolding({
      'refs/heads/main': 'main content',
      'refs/heads/topic': 'topic content',
      'refs/heads/release': 'release content',
      'refs/tags/v1': 'v1 content',
      'refs/tags/v2': 'v2 content',
    });
    const oids = await seed();
    const store = repos();
    await store.create('target');

    const { progress } = await runToCompletion(store, {
      repository: 'target',
      gitUrl: remote.url,
      fetch: remote.fetch,
    });

    expect(progress.done).toBe(true);
    const refs = await store.listRefs('target');
    expect(Object.fromEntries(refs.map((ref) => [ref.name, ref.oid]))).toEqual(oids);
    expect(progress.head).toBe('refs/heads/main');
  });

  it('needs no credential for a public remote, and shows none', async () => {
    const { remote, seed } = remoteHolding({ 'refs/heads/main': 'public content' });
    await seed();
    const store = repos();
    await store.create('target');

    const { progress } = await runToCompletion(store, {
      repository: 'target',
      gitUrl: remote.url,
      fetch: remote.fetch,
    });

    expect(progress.done).toBe(true);
    expect(remote.seen()).toEqual([]);
  });
});

describe('copying a private repository in', () => {
  it('shows the remote the stored credential and keeps it out of everything else', async () => {
    const { remote, seed } = remoteHolding(
      { 'refs/heads/main': 'private content' },
      { requires: `Bearer ${TOKEN}` },
    );
    await seed();
    const store = repos();
    await store.create('target');
    const { log, records } = recordingLog();

    const { progress } = await runToCompletion(store, {
      repository: 'target',
      gitUrl: remote.url,
      credential: { kind: 'token', token: TOKEN },
      fetch: remote.fetch,
      log,
    });

    expect(progress.done).toBe(true);
    expect(remote.seen()).toContain(`Bearer ${TOKEN}`);
    expect(records.join('\n')).not.toContain(TOKEN);
    expect(JSON.stringify(progress)).not.toContain(TOKEN);
  });

  it('fails when it has no credential for a remote that wants one', async () => {
    const { remote, seed } = remoteHolding(
      { 'refs/heads/main': 'private content' },
      { requires: `Bearer ${TOKEN}` },
    );
    await seed();
    const store = repos();
    await store.create('target');

    await expect(
      copyIn(store, { repository: 'target', gitUrl: remote.url, fetch: remote.fetch }),
    ).rejects.toThrow();
    expect(await store.listRefs('target')).toEqual([]);
  });

  it.each([
    ['a token', { kind: 'token' as const, token: TOKEN }, `Bearer ${TOKEN}`],
    [
      'a basic pair',
      { kind: 'basic' as const, username: 'a-user', password: 'a-password' },
      `Basic ${Buffer.from('a-user:a-password').toString('base64')}`,
    ],
  ])('authenticates with %s as git does', (_name, credential, expected) => {
    expect(authorizationFor(credential)).toBe(expected);
  });
});

describe('checking a credential against the remote before anything is written', () => {
  it('says accepted when the remote takes it', async () => {
    const { remote, seed } = remoteHolding(
      { 'refs/heads/main': 'content' },
      { requires: `Bearer ${TOKEN}` },
    );
    await seed();

    expect(
      await checkRemoteCredential({
        gitUrl: remote.url,
        credential: { kind: 'token', token: TOKEN },
        fetch: remote.fetch,
      }),
    ).toBe('accepted');
  });

  it.each([401, 403])('says refused when the remote answers %i', async (status) => {
    const { remote } = remoteHolding({}, { fails: status });

    expect(await checkRemoteCredential({ gitUrl: remote.url, fetch: remote.fetch })).toBe(
      'refused',
    );
  });

  it.each([
    ['the remote is down', { fails: 500 }],
    ['the remote cannot be reached', { unreachable: true }],
    ['the remote has no such repository', { fails: 404 }],
  ])('says unreachable when %s', async (_name, options) => {
    const { remote } = remoteHolding({}, options);

    expect(await checkRemoteCredential({ gitUrl: remote.url, fetch: remote.fetch })).toBe(
      'unreachable',
    );
  });
});

describe('a copy too large for one unit of work', () => {
  it('resumes at the refs it has not brought, and asks for nothing twice', async () => {
    const refs = Object.fromEntries(
      Array.from({ length: 7 }, (_, i) => [
        `refs/heads/branch-${String(i)}`,
        `content ${String(i)}`,
      ]),
    );
    const { remote, seed } = remoteHolding(refs);
    const oids = await seed();
    const store = repos();
    await store.create('target');

    const { units, progress, asked } = await runToCompletion(store, {
      repository: 'target',
      gitUrl: remote.url,
      fetch: remote.fetch,
      batchSize: 2,
    });

    expect(progress.done).toBe(true);
    expect(units).toBeGreaterThan(1);

    const stored = await store.listRefs('target');
    expect(Object.fromEntries(stored.map((ref) => [ref.name, ref.oid]))).toEqual(oids);

    // Each unit of work started from strictly more than the last.
    for (let i = 1; i < asked.length; i += 1) {
      expect((asked[i] ?? []).length).toBeGreaterThan((asked[i - 1] ?? []).length);
    }
  });
});

describe('a copy that fails part way', () => {
  it('leaves what it brought and says what went wrong, without the credential', async () => {
    const { remote, seed } = remoteHolding(
      { 'refs/heads/main': 'content' },
      { fails: 500, requires: `Bearer ${TOKEN}` },
    );
    await seed();
    const store = repos();
    await store.create('target');

    const failure = await copyIn(store, {
      repository: 'target',
      gitUrl: remote.url,
      credential: { kind: 'token', token: TOKEN },
      fetch: remote.fetch,
    }).catch((err: Error) => err);

    expect(failure).toBeInstanceOf(Error);
    expect((failure as Error).message).not.toContain(TOKEN);
  });
});

async function createAgainst(
  options: Parameters<typeof fakeRemote>[0],
  body: Record<string, unknown> = {},
) {
  const remote = fakeRemote(options);
  cleanups.push(remote.dispose);
  await remote.put('refs/heads/main', 'content');

  const dir = mkdtempSync(join(tmpdir(), 'parley-create-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repoStore = localRepositoryStore(dir);
  const service = await identityService([], { repos: repoStore, fetch: remote.fetch });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  await seedOrg(service.store, owner);
  const key = (await seedApiKey(service.store, owner)).presented;

  const response = await service.app.request(
    `${MOUNT}/workspaces`,
    withJson('POST', { slug: 'a-project', git_url: remote.url, ...body }, key),
  );
  return { response, service, repoStore };
}

describe('creating a workspace against a remote', () => {
  it('answers 400 for a credential the remote refuses, writing nothing at all', async () => {
    const { response, service, repoStore } = await createAgainst(
      { requires: 'Bearer the-right-one' },
      { git_pat: TOKEN },
    );

    expect(response.status).toBe(400);
    expect(await response.text()).not.toContain(TOKEN);
    expect(await service.store.all('SELECT slug FROM workspaces')).toEqual([]);
    expect(await service.store.all("SELECT key FROM entries WHERE kind = 'secret'")).toEqual([]);
    expect(await repoStore.exists('a-project')).toBe(false);
  });

  it.each([
    ['the remote is down', { fails: 500 }],
    ['the remote cannot be reached', { unreachable: true }],
  ])('answers 201 when %s, leaving the copy to find out', async (_name, options) => {
    const { response, service } = await createAgainst(options);

    expect(response.status).toBe(201);
    expect(((await response.json()) as { status: string }).status).toBe('pending');
    expect(await service.store.all('SELECT slug FROM workspaces')).toHaveLength(1);
  });

  it('answers 201 when the remote takes the credential it was given', async () => {
    const { response } = await createAgainst({ requires: `Bearer ${TOKEN}` }, { git_pat: TOKEN });

    expect(response.status).toBe(201);
  });
});
