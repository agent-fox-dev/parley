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

/** Everything the suite does to a list, for the contiguity property. */
async function change(place: World, id: string, body: unknown): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches/${id}`, {
    method: 'PATCH',
    headers: {
      authorization: `Bearer ${place.credential}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

async function readOne(place: World, id: string): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches/${id}`, {
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

async function listAll(place: World): Promise<Array<Record<string, unknown>>> {
  const response = await place.service.app.request(`/api/v1/workspaces/${place.slug}/patches`, {
    headers: { authorization: `Bearer ${place.credential}` },
  });
  return (await response.json()) as Array<Record<string, unknown>>;
}

/** Three patches, so the list has an order worth disturbing. */
async function three(place: World): Promise<string[]> {
  const ids: string[] = [];
  for (const branch of ['one', 'two', 'three']) {
    const response = await add(place, { branch_name: branch, skip_branch_check: true });
    ids.push(((await response.json()) as { id: string }).id);
  }
  return ids;
}

describe('the list a client reads', () => {
  it('is in position order, without the deleted ones', async () => {
    const place = await world();
    const ids = await three(place);
    await place.service.store.run('UPDATE patches SET status = ? WHERE id = ?', [
      'deleted',
      ids[1],
    ]);

    const listed = await listAll(place);

    expect(listed.map((patch) => patch['branch_name'])).toEqual(['one', 'three']);
  });

  it('is empty for a standard workspace', async () => {
    const place = await world({ workspace_mode: 'standard' });

    expect(await listAll(place)).toEqual([]);
  });
});

describe('one patch read on its own', () => {
  it('is readable even once it is deleted', async () => {
    const place = await world();
    const [id] = await three(place);
    await place.service.store.run('UPDATE patches SET status = ? WHERE id = ?', ['deleted', id]);

    const response = await readOne(place, id ?? '');

    expect(response.status).toBe(200);
    expect(((await response.json()) as Record<string, unknown>)['status']).toBe('deleted');
  });

  it('carries the tip the hub kept when it replaced the branch', async () => {
    const place = await world();
    const response = await add(place, { branch_name: 'here' });
    const id = ((await response.json()) as { id: string }).id;
    const kept = (await place.repos.listRefs(place.repository))[0]?.oid ?? '';
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/hub/replaced/here', oldOid: null, newOid: kept, force: true },
    ]);

    const read = (await (await readOne(place, id)).json()) as Record<string, unknown>;

    expect(read['replaced_sha']).toBe(kept);
    expect(String(read['replaced_sha'])).toMatch(/^[0-9a-f]{40}$/);
  });

  it('omits it where there is none, and where the lookup fails', async () => {
    const place = await world();
    const id = ((await (await add(place, { branch_name: 'here' })).json()) as { id: string }).id;

    expect(
      ((await (await readOne(place, id)).json()) as Record<string, unknown>)['replaced_sha'],
    ).toBeUndefined();

    // A store that cannot answer is not a reason to fail the read.
    const original = place.repos.listRefs.bind(place.repos);
    place.repos.listRefs = (() =>
      Promise.reject(new Error('the store is unavailable'))) as typeof place.repos.listRefs;
    const response = await readOne(place, id);
    place.repos.listRefs = original;

    expect(response.status).toBe(200);
    expect(((await response.json()) as Record<string, unknown>)['replaced_sha']).toBeUndefined();
  });

  it('answers 404 for a patch nobody has', async () => {
    const place = await world();

    expect((await readOne(place, 'no-such-patch')).status).toBe(404);
  });
});

describe('a change to one patch', () => {
  it('changes what it names and leaves the rest alone', async () => {
    const place = await world();
    const [id] = await three(place);

    const response = await change(place, id ?? '', {
      description: 'what this patch is for',
      upstream_pr_url: 'https://example.test/pull/42',
      status: 'disabled',
      branch_name: 'something-else',
    });
    const patch = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(patch['description']).toBe('what this patch is for');
    expect(patch['upstream_pr_url']).toBe('https://example.test/pull/42');
    expect(patch['status']).toBe('disabled');
    // The branch is not a thing a change may change.
    expect(patch['branch_name']).toBe('one');
  });

  it('moves a patch and keeps the positions 1 to n', async () => {
    const place = await world();
    const ids = await three(place);

    await change(place, ids[2] ?? '', { position: 1 });

    const listed = await listAll(place);
    expect(listed.map((patch) => patch['branch_name'])).toEqual(['three', 'one', 'two']);
    expect(listed.map((patch) => patch['position'])).toEqual([1, 2, 3]);
  });

  it.each([
    ['a status nobody has', { status: 'wandering' }, 400],
    ['the deleted status', { status: 'deleted' }, 400],
    ['a position below one', { position: 0 }, 400],
    ['a position past the end', { position: 9 }, 400],
    ['a description that is a number', { description: 7 }, 400],
  ])('is refused for %s', async (_what, body, expected) => {
    const place = await world();
    const ids = await three(place);
    const before = await listAll(place);

    expect((await change(place, ids[0] ?? '', body)).status).toBe(expected);
    expect(await listAll(place)).toEqual(before);
  });

  it('answers 404 for an unknown patch and 409 for a deleted one', async () => {
    const place = await world();
    const [id] = await three(place);

    expect((await change(place, 'nobody', { status: 'disabled' })).status).toBe(404);

    await place.service.store.run('UPDATE patches SET status = ? WHERE id = ?', ['deleted', id]);
    expect((await change(place, id ?? '', { status: 'disabled' })).status).toBe(409);
  });
});

describe('the positions of a list under any sequence of operations', () => {
  it('are always 1 to n, with none repeated and none skipped', async () => {
    const place = await world();
    const ids = await three(place);

    // A deterministic walk over the operations that can disturb an
    // order: inserts at every position, moves, a disable, a delete.
    const operations: Array<() => Promise<unknown>> = [
      () => add(place, { branch_name: 'four', position: 2, skip_branch_check: true }),
      () => change(place, ids[0] ?? '', { position: 3 }),
      () => add(place, { branch_name: 'five', position: 1, skip_branch_check: true }),
      () => change(place, ids[2] ?? '', { position: 1 }),
      () => change(place, ids[1] ?? '', { status: 'disabled' }),
      () => add(place, { branch_name: 'six', position: 99, skip_branch_check: true }),
      () => change(place, ids[2] ?? '', { position: 4 }),
    ];

    for (const [index, operation] of operations.entries()) {
      await operation();
      const held = (await listAll(place)).map((patch) => patch['position']);
      expect(held, `after operation ${String(index)}`).toEqual(
        Array.from({ length: held.length }, (_unused, at) => at + 1),
      );
    }
  });
});

async function remove(place: World, id: string): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches/${id}`, {
    method: 'DELETE',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

async function restore(place: World, id: string): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches/${id}/restore`, {
    method: 'POST',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

async function reorder(place: World, body: unknown): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/patches/reorder`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${place.credential}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

describe('a patch removed from the list', () => {
  it('closes the positions behind it and discards its kept tip', async () => {
    const place = await world();
    const ids = await three(place);
    const kept = (await place.repos.listRefs(place.repository))[0]?.oid ?? '';
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/hub/replaced/one', oldOid: null, newOid: kept, force: true },
    ]);

    const response = await remove(place, ids[0] ?? '');

    expect(response.status).toBe(204);
    const listed = await listAll(place);
    expect(listed.map((patch) => patch['branch_name'])).toEqual(['two', 'three']);
    expect(listed.map((patch) => patch['position'])).toEqual([1, 2]);
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.some((ref) => ref.name === 'refs/hub/replaced/one')).toBe(false);
  });

  it('answers the same when the kept tip cannot be discarded', async () => {
    const place = await world();
    const ids = await three(place);
    const original = place.repos.updateRefs.bind(place.repos);
    place.repos.updateRefs = (() =>
      Promise.reject(new Error('the store will not take it'))) as typeof place.repos.updateRefs;

    const response = await remove(place, ids[0] ?? '');
    place.repos.updateRefs = original;

    expect(response.status).toBe(204);
    expect(await listAll(place)).toHaveLength(2);
  });
});

describe('a removed patch restored', () => {
  it('comes back active, at the end, with its removal forgotten', async () => {
    const place = await world();
    const ids = await three(place);
    await remove(place, ids[0] ?? '');

    const response = await restore(place, ids[0] ?? '');
    const patch = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(patch['status']).toBe('active');
    expect(patch['deleted_at']).toBeNull();
    expect(patch['position']).toBe(3);
    expect((await listAll(place)).map((one) => one['position'])).toEqual([1, 2, 3]);
  });

  it('is refused for a patch that was never removed', async () => {
    const place = await world();
    const ids = await three(place);

    expect((await restore(place, ids[0] ?? '')).status).toBe(400);
  });

  it('answers 404 for a patch nobody has', async () => {
    const place = await world();

    expect((await restore(place, 'nobody')).status).toBe(404);
  });
});

describe('a reorder of the whole list', () => {
  it('sets every position in the order it was given', async () => {
    const place = await world();
    const ids = await three(place);

    const response = await reorder(place, { ids: [ids[2], ids[0], ids[1]] });
    const listed = (await response.json()) as Array<Record<string, unknown>>;

    expect(response.status).toBe(200);
    expect(listed.map((patch) => patch['branch_name'])).toEqual(['three', 'one', 'two']);
    expect(listed.map((patch) => patch['position'])).toEqual([1, 2, 3]);
  });

  it.each([
    ['an empty order while patches exist', () => ({ ids: [] as string[] })],
    ['one that leaves a patch out', (ids: string[]) => ({ ids: [ids[0], ids[1]] })],
    ['one that names a patch twice', (ids: string[]) => ({ ids: [ids[0], ids[0], ids[1]] })],
    ['one that names a stranger', (ids: string[]) => ({ ids: [ids[0], ids[1], 'elsewhere'] })],
    ['a body that is not an order at all', () => ({ order: 'please' })],
  ])('is refused for %s, and nothing moves', async (_what, make: (ids: string[]) => unknown) => {
    const place = await world();
    const ids = await three(place);
    const before = await listAll(place);

    const response = await reorder(place, make(ids));

    expect(response.status).toBe(400);
    expect(await listAll(place)).toEqual(before);
  });
});
