import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService, recordedEvents } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The outcomes of a standard sync, each against a remote that really
 * serves the protocol. What tells them apart is not the status alone
 * but what the workspace records afterwards, so every case reads the
 * workspace back.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-origin';

interface World {
  service: Service;
  remote: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
  /** Where the workspace's branch stands in the hub's own copy. */
  local: string;
}

/** A ready workspace whose repository and origin share one commit. */
async function world(options: { remote?: FakeRemote; credential?: boolean } = {}): Promise<World> {
  const remote = options.remote ?? fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-sync-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, fetch: remote.fetch });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);

  const first = await buildCommit(
    remote.store,
    remote.repository,
    { 'a.txt': 'first\n' },
    [],
    'first',
  );
  await remote.store.updateRefs(remote.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid, force: true },
  ]);

  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    branch: 'main',
    git_url: `${remote.url}/origin.git`,
    head_sha: first.oid,
    upstream_head_sha: first.oid,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const here = await buildCommit(repos, repository, { 'a.txt': 'first\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: here.oid, force: true },
  ]);

  if (options.credential !== false) {
    await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);
  }

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    local: here.oid,
  };
}

/** Adds commits to origin, on top of what it holds or of another commit. */
async function advanceOrigin(place: World, count: number, from?: string): Promise<string> {
  let parent =
    from ??
    (await place.remote.store.listRefs(place.remote.repository)).find(
      (ref) => ref.name === 'refs/heads/main',
    )?.oid;
  for (let i = 0; i < count; i += 1) {
    const built = await buildCommit(
      place.remote.store,
      place.remote.repository,
      { 'a.txt': `first\nand ${String(i)}\n` },
      parent === undefined ? [] : [parent],
      `origin ${String(i)}`,
    );
    parent = built.oid;
  }
  await place.remote.store.updateRefs(place.remote.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: parent ?? '', force: true },
  ]);
  return parent ?? '';
}

/**
 * Rewrites origin's history onto a root of its own, which is what a
 * force-push looks like from here: a head that descends from nothing
 * the workspace has.
 */
async function rewriteOrigin(place: World): Promise<string> {
  const built = await buildCommit(
    place.remote.store,
    place.remote.repository,
    { 'a.txt': 'a history of its own\n' },
    [],
    'rewritten',
  );
  await place.remote.store.updateRefs(place.remote.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

async function sync(place: World, query = ''): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/sync${query}`, {
    method: 'POST',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

async function read(place: World): Promise<Record<string, unknown>> {
  const response = await place.service.app.request(`/api/v1/workspaces/${place.slug}`, {
    headers: { authorization: `Bearer ${place.credential}` },
  });
  return (await response.json()) as Record<string, unknown>;
}

/** Where the workspace's own branch stands in the hub's repository. */
async function branchHere(place: World): Promise<string | undefined> {
  return (await place.repos.listRefs(place.repository)).find(
    (ref) => ref.name === 'refs/heads/main',
  )?.oid;
}

describe('a sync of a standard workspace', () => {
  it('reports it up to date when origin is where the workspace is', async () => {
    const place = await world();
    const before = await read(place);

    const response = await sync(place);
    const after = await read(place);

    expect(response.status).toBe(200);
    expect(after['head_sha']).toBe(before['head_sha']);
    expect(after['sync_error']).toBeNull();
    expect(after['last_sync_at']).not.toBeNull();
  });

  it('fast-forwards the branch when origin has advanced', async () => {
    const place = await world();
    const head = await advanceOrigin(place, 2, place.local);

    const response = await sync(place);
    const after = await read(place);

    expect(response.status).toBe(200);
    expect(after['head_sha']).toBe(head);
    expect(after['upstream_head_sha']).toBe(head);
    expect(after['sync_error']).toBeNull();
    // The repository moved, not only the record.
    expect(await branchHere(place)).toBe(head);
    // And the hub wrote down what it did, with the result it reached.
    expect(await recordedEvents(place.service.store, 'hub.workspace.sync')).toEqual([
      expect.objectContaining({
        action: 'sync',
        workspace: place.slug,
        metadata: { result: 'fast_forward' },
      }),
    ]);
  });

  it('refuses a diverged origin and says how to recover', async () => {
    const place = await world();
    const before = await read(place);
    await rewriteOrigin(place);

    const response = await sync(place);
    const after = await read(place);

    expect(response.status).toBe(409);
    // A sync that was refused did nothing, so it recorded nothing.
    expect(await recordedEvents(place.service.store, 'hub.workspace.sync')).toEqual([]);
    expect(String(after['sync_error'])).toContain('reset_to_upstream');
    expect(after['head_sha']).toBe(before['head_sha']);
    expect(await branchHere(place)).toBe(place.local);
  });

  it('moves the branch anyway when asked to reset to upstream', async () => {
    const place = await world();
    const head = await rewriteOrigin(place);

    const response = await sync(place, '?reset_to_upstream=true');
    const after = await read(place);

    expect(response.status).toBe(200);
    expect(after['head_sha']).toBe(head);
    expect(after['upstream_head_sha']).toBe(head);
    expect(after['sync_error']).toBeNull();
    expect(await branchHere(place)).toBe(head);
  });

  it('clears an earlier error when it next succeeds', async () => {
    const place = await world();
    await rewriteOrigin(place);

    expect((await sync(place)).status).toBe(409);
    expect(await read(place).then((workspace) => workspace['sync_error'])).not.toBeNull();

    // Recovered the way the error said to.
    expect((await sync(place, '?reset_to_upstream=true')).status).toBe(200);
    expect(await read(place).then((workspace) => workspace['sync_error'])).toBeNull();
  });
});

describe('a sync that cannot reach origin', () => {
  it.each([
    ['an unreachable origin', { unreachable: true }, true],
    ['an origin that refuses the credential', { requires: 'Bearer something-else' }, true],
    ['a workspace with no credential at all', { requires: `Bearer ${TOKEN}` }, false],
  ])('answers 502 and records why: %s', async (_what, options, credential) => {
    const remote = fakeRemote(options);
    const place = await world({ remote, credential });
    const before = await read(place);

    const response = await sync(place);
    const after = await read(place);

    expect(response.status).toBe(502);
    expect(after['sync_error']).not.toBeNull();
    expect(String(after['sync_error'])).not.toContain(TOKEN);
    expect(after['head_sha']).toBe(before['head_sha']);
    expect(after['upstream_head_sha']).toBe(before['upstream_head_sha']);
  });

  it('answers 504 when the sync runs past its deadline, and recovers after', async () => {
    let slow = true;
    const base = fakeRemote({ requires: `Bearer ${TOKEN}` });
    const remote: FakeRemote = {
      ...base,
      fetch: (async (input: string | URL | Request, init?: RequestInit) => {
        if (slow) {
          await new Promise((resolve) => setTimeout(resolve, 60));
        }
        return base.fetch(input, init);
      }) as unknown as typeof fetch,
    };
    const place = await world({ remote });

    const response = await sync(place, '?deadline_ms=10');
    expect(response.status).toBe(504);
    expect(await read(place).then((workspace) => workspace['sync_error'])).not.toBeNull();

    slow = false;
    expect((await sync(place)).status).toBe(200);
  });
});

describe('when a sync may run at all', () => {
  it.each([
    ['pending', { status: 'pending' as const }],
    ['cloning', { status: 'cloning' as const }],
    ['failed', { status: 'failed' as const }],
    ['archived', { status: 'archived' as const }],
    ['ready with syncing switched off', { settings: { sync_enabled: false } }],
  ])('refuses a workspace that is %s, and asks origin nothing', async (_what, over) => {
    const place = await world();
    await place.service.store.run(`UPDATE workspaces SET status = ?, settings = ? WHERE slug = ?`, [
      (over as { status?: string }).status ?? 'ready',
      JSON.stringify((over as { settings?: unknown }).settings ?? {}),
      place.slug,
    ]);
    const before = await read(place);
    const asked = place.remote.requests();

    const response = await sync(place);

    expect(response.status).toBe(400);
    expect(place.remote.requests()).toBe(asked);
    expect(await read(place)).toEqual(before);
  });

  it('refuses at once while another operation holds the workspace', async () => {
    const place = await world();
    const held = await place.service.locks.take(`workspace:${place.slug}`);
    expect(held, 'the suite took the lock').toBeDefined();

    const started = Date.now();
    const refused = await sync(place);
    const waited = Date.now() - started;

    expect(refused.status).toBe(409);
    expect(((await refused.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
    // It did not queue behind the lock: it answered.
    expect(waited).toBeLessThan(2000);

    await held?.release();
    expect((await sync(place)).status).toBe(200);
  });

  it('leaves the status at ready while it runs, so busy is the lock and not a state', async () => {
    const place = await world();
    await advanceOrigin(place, 1, place.local);

    // Read the workspace from inside the sync, which is the only
    // moment at which a stored "syncing" status could be seen.
    let seen: unknown;
    let second: Response | undefined;
    const slow = new Promise<void>((resolve) => {
      setTimeout(() => {
        void (async () => {
          seen = (await read(place))['status'];
          second = await sync(place);
          resolve();
        })();
      }, 0);
    });

    const [response] = await Promise.all([sync(place), slow]);

    expect(response.status).toBe(200);
    expect(seen).toBe('ready');
    if (second !== undefined && second.status !== 200) {
      expect(second.status).toBe(409);
      expect(((await second.json()) as { error: { error_type: string } }).error.error_type).toBe(
        'workspace_busy',
      );
    }
  });
});
