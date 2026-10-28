import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { repositoryOf } from '../repo/repository.js';
import { livePatches } from '../patch/list.js';
import { type Service, identityService } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Adding patches. The order of the checks matters here as much as in
 * the branch push: a batch tells a client about the cheap mistake
 * before it goes looking for branches.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-the-fork';

interface World {
  service: Service;
  remote: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
}

async function world(over: object = {}, options: { remote?: FakeRemote } = {}): Promise<World> {
  const remote = options.remote ?? fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-patches-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, fetch: remote.fetch });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    git_url: `${remote.url}/fork.git`,
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/here', oldOid: null, newOid: first.oid, force: true },
    // The fork's copy of another branch, already fetched.
    { name: 'refs/hub/origin/tracked', oldOid: null, newOid: first.oid, force: true },
  ]);
  await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
  };
}

async function add(place: World, body: unknown): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${place.credential}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

async function positions(place: World): Promise<Array<[string, number]>> {
  return (await livePatches(place.service.store, place.slug)).map((patch) => [
    patch.branch_name,
    patch.position,
  ]);
}

describe('a patch added to the list', () => {
  it('is appended as active, and says it found the branch locally', async () => {
    const place = await world();

    const response = await add(place, { branch_name: 'here' });
    const patch = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(201);
    expect(patch['branch_name']).toBe('here');
    expect(patch['status']).toBe('active');
    expect(patch['position']).toBe(1);
    expect(patch['branch_resolution']).toBe('local');
  });

  it("creates the branch from the fork's fetched copy when that is where it is", async () => {
    const place = await world();

    const response = await add(place, { branch_name: 'tracked' });

    expect(response.status).toBe(201);
    expect(((await response.json()) as Record<string, unknown>)['branch_resolution']).toBe(
      'origin_tracking',
    );
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.some((ref) => ref.name === 'refs/heads/tracked')).toBe(true);
  });

  it('reports skipped and writes no branch when the check is skipped', async () => {
    const place = await world();

    const response = await add(place, { branch_name: 'nowhere', skip_branch_check: true });

    expect(response.status).toBe(201);
    expect(((await response.json()) as Record<string, unknown>)['branch_resolution']).toBe(
      'skipped',
    );
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.some((ref) => ref.name === 'refs/heads/nowhere')).toBe(false);
  });

  it('goes to the fork only where the workspace takes its patches from it', async () => {
    const hub = await world();
    expect((await add(hub, { branch_name: 'only-on-the-fork' })).status).toBe(400);
    expect(hub.remote.requests()).toBe(0);

    const fromFork = await world({ settings: { patch_branch_source: 'origin' } });
    const tip = await buildCommit(
      fromFork.remote.store,
      fromFork.remote.repository,
      { 'f.txt': 'on the fork\n' },
      [],
      'fork only',
    );
    await fromFork.remote.store.updateRefs(fromFork.remote.repository, [
      { name: 'refs/heads/only-on-the-fork', oldOid: null, newOid: tip.oid, force: true },
    ]);

    const response = await add(fromFork, { branch_name: 'only-on-the-fork' });

    expect(response.status).toBe(201);
    expect(((await response.json()) as Record<string, unknown>)['branch_resolution']).toBe(
      'origin_fetch',
    );
    const refs = await fromFork.repos.listRefs(fromFork.repository);
    expect(refs.find((ref) => ref.name === 'refs/heads/only-on-the-fork')?.oid).toBe(tip.oid);
  });
});

describe('where a patch lands in the list', () => {
  it('inserts at a position and moves the rest down, and appends past the end', async () => {
    const place = await world();
    for (const branch of ['one', 'two', 'three']) {
      await place.repos.updateRefs(place.repository, [
        {
          name: `refs/heads/${branch}`,
          oldOid: null,
          newOid: (await place.repos.listRefs(place.repository))[0]?.oid ?? '',
          force: true,
        },
      ]);
      await add(place, { branch_name: branch });
    }
    expect(await positions(place)).toEqual([
      ['one', 1],
      ['two', 2],
      ['three', 3],
    ]);

    await place.repos.updateRefs(place.repository, [
      {
        name: 'refs/heads/first',
        oldOid: null,
        newOid: (await place.repos.listRefs(place.repository))[0]?.oid ?? '',
        force: true,
      },
    ]);
    await add(place, { branch_name: 'first', position: 1 });

    expect(await positions(place)).toEqual([
      ['first', 1],
      ['one', 2],
      ['two', 3],
      ['three', 4],
    ]);

    await add(place, { branch_name: 'far', position: 99, skip_branch_check: true });
    await add(place, { branch_name: 'last', skip_branch_check: true });

    expect((await positions(place)).map(([, position]) => position)).toEqual([1, 2, 3, 4, 5, 6]);
    expect((await positions(place)).at(-1)).toEqual(['last', 6]);
  });
});

describe('a branch already in the list', () => {
  it('answers with the existing patch for if_not_exists, and 409 without it', async () => {
    const place = await world();
    const first = (await (await add(place, { branch_name: 'here' })).json()) as { id: string };

    const again = await add(place, { branch_name: 'here', if_not_exists: true });
    const strict = await add(place, { branch_name: 'here' });

    expect(again.status).toBe(200);
    expect(((await again.json()) as { id: string }).id).toBe(first.id);
    expect(strict.status).toBe(409);
    expect(await positions(place)).toHaveLength(1);
  });
});

describe('an add the route will not take', () => {
  it.each([
    ['no branch name', {}],
    ['an empty branch name', { branch_name: '' }],
    ['the integration branch', { branch_name: 'integration' }],
    ['a position below one', { branch_name: 'here', position: 0 }],
    ['a branch nobody has', { branch_name: 'nowhere' }],
  ])('answers 400 and changes nothing: %s', async (_what, body) => {
    const place = await world();

    const response = await add(place, body);

    expect(response.status).toBe(400);
    expect(await positions(place)).toEqual([]);
  });

  it.each([
    ['a standard workspace', { workspace_mode: 'standard' as const }],
    ['an archived one', { status: 'archived' as const }],
  ])('answers 400 for %s', async (_what, over) => {
    const place = await world(over);

    expect((await add(place, { branch_name: 'here' })).status).toBe(400);
  });

  it('refuses a branch named twice in one batch', async () => {
    const place = await world();

    const response = await add(place, [{ branch_name: 'here' }, { branch_name: 'here' }]);

    expect(response.status).toBe(409);
    expect(await positions(place)).toEqual([]);
  });

  it('refuses at once while another operation holds the workspace', async () => {
    const place = await world();
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await add(place, { branch_name: 'here' });

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
    await held?.release();
  });

  it('answers 502 with origin_fetch_failed when the fork cannot be read', async () => {
    const refusing = fakeRemote({ fails: 500 });
    const place = await world(
      { settings: { patch_branch_source: 'origin' } },
      { remote: refusing },
    );

    const response = await add(place, { branch_name: 'only-on-the-fork' });

    expect(response.status).toBe(502);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'origin_fetch_failed',
    );
    expect(await positions(place)).toEqual([]);
  });
});

describe('a batch of patches', () => {
  it("answers for the second element's syntax before the first element's missing branch", async () => {
    const place = await world();

    const response = await add(place, [
      { branch_name: 'nowhere' },
      { branch_name: 'here', position: 0 },
    ]);
    const message = ((await response.json()) as { error: { message: string } }).error.message;

    expect(response.status).toBe(400);
    expect(message).toContain('patch[1]: position must be >= 1');
  });

  it('is refused whole on the first branch failure, leaving what it created', async () => {
    const place = await world();

    const response = await add(place, [{ branch_name: 'tracked' }, { branch_name: 'nowhere' }]);

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      'patch[1]',
    );
    expect(await positions(place)).toEqual([]);

    // The branch created for the first element stays, and is local now.
    const retry = await add(place, { branch_name: 'tracked' });
    expect(((await retry.json()) as Record<string, unknown>)['branch_resolution']).toBe('local');
  });

  it('adds every element when they all resolve', async () => {
    const place = await world();

    const response = await add(place, [{ branch_name: 'here' }, { branch_name: 'tracked' }]);

    expect(response.status).toBe(201);
    expect((await response.json()) as unknown[]).toHaveLength(2);
    expect(await positions(place)).toEqual([
      ['here', 1],
      ['tracked', 2],
    ]);
  });
});
