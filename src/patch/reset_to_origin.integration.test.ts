import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedPatch, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Setting a patch branch back to the fork's, against a fork that
 * really serves the protocol. Each rule is exercised by putting the
 * two branches in the state that rule is for, and the suite reads the
 * repository afterwards: what the route says it did and what it did
 * have to agree.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-the-fork';
const PATCH = 'a-patch';

interface World {
  service: Service;
  remote: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
  base: string;
}

async function world(over: object = {}, options: { remote?: FakeRemote } = {}): Promise<World> {
  const remote = options.remote ?? fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-reset-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, fetch: remote.fetch, jobs: 'store' });
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
    settings: { patch_branch_source: 'origin', auto_rebuild_after_sync: true },
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);
  await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

/** The patch's branch in the hub's copy, where the suite wants one. */
async function here(
  place: World,
  files: Record<string, string>,
  from?: string,
  message = 'in the hub',
): Promise<string> {
  const built = await buildCommit(
    place.repos,
    place.repository,
    files,
    from === undefined ? [] : [from],
    message,
  );
  await place.repos.updateRefs(place.repository, [
    { name: `refs/heads/${PATCH}`, oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

/** The same branch on the fork. */
async function onFork(
  place: World,
  files: Record<string, string>,
  from?: string,
  message = 'on the fork',
): Promise<string> {
  const parents = from === undefined ? [] : [from];
  const built = await buildCommit(
    place.remote.store,
    place.remote.repository,
    files,
    parents,
    message,
  );
  await place.remote.store.updateRefs(place.remote.repository, [
    { name: `refs/heads/${PATCH}`, oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

async function reset(place: World, id: string): Promise<Response> {
  return place.service.app.request(
    `/api/v1/workspaces/${place.slug}/patches/${id}/reset-to-origin`,
    { method: 'POST', headers: { authorization: `Bearer ${place.credential}` } },
  );
}

async function patchId(place: World, over: object = {}): Promise<string> {
  await seedPatch(place.service.store, place.slug, PATCH, over);
  const rows = await place.service.store.all<{ id: string }>(
    'SELECT id FROM patches WHERE workspace_slug = ? AND branch_name = ?',
    [place.slug, PATCH],
  );
  return rows[0]?.id ?? '';
}

async function branchHere(place: World): Promise<string | undefined> {
  return (await place.repos.listRefs(place.repository)).find(
    (ref) => ref.name === `refs/heads/${PATCH}`,
  )?.oid;
}

describe('resetting a patch to the fork', () => {
  it('creates the branch where the hub has none', async () => {
    const place = await world();
    const theirs = await onFork(place, { 'p.txt': 'from the fork\n' });
    const id = await patchId(place);

    const response = await reset(place, id);
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(body['action']).toBe('created');
    expect(body['replaced_sha']).toBeUndefined();
    expect(await branchHere(place)).toBe(theirs);
  });

  it('writes nothing where the two already agree', async () => {
    const place = await world();
    // The same content, author and message is the same commit on
    // both sides, which is what "already in sync" means.
    const theirs = await onFork(place, { 'p.txt': 'the same\n' }, undefined, 'the same commit');
    const mine = await here(place, { 'p.txt': 'the same\n' }, undefined, 'the same commit');
    expect(mine).toBe(theirs);
    const id = await patchId(place);
    const before = await place.repos.listRefs(place.repository);

    const response = await reset(place, id);

    expect(response.status).toBe(200);
    expect(((await response.json()) as Record<string, unknown>)['action']).toBe('none');
    expect(await place.repos.listRefs(place.repository)).toEqual(before);
  });

  it('fast-forwards where the fork has moved ahead', async () => {
    const place = await world();
    const mine = await here(place, { 'p.txt': 'one\n' }, undefined);
    const theirs = await onFork(place, { 'p.txt': 'one\nand two\n' }, mine);
    const id = await patchId(place);

    const response = await reset(place, id);
    const body = (await response.json()) as Record<string, unknown>;

    expect(body['action']).toBe('fast_forwarded');
    expect(body['replaced_sha']).toBeUndefined();
    expect(await branchHere(place)).toBe(theirs);
  });

  it('replaces a diverged branch and keeps the tip it dropped', async () => {
    const place = await world();
    const mine = await here(place, { 'p.txt': 'the hub went this way\n' }, undefined);
    const theirs = await onFork(place, { 'p.txt': 'the fork went that way\n' });
    const id = await patchId(place);

    const response = await reset(place, id);
    const body = (await response.json()) as Record<string, unknown>;

    expect(body['action']).toBe('replaced');
    expect(body['replaced_sha']).toBe(mine);
    expect(await branchHere(place)).toBe(theirs);

    // The dropped tip is recoverable.
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.find((ref) => ref.name === `refs/hub/replaced/${PATCH}`)?.oid).toBe(mine);
  });

  it('supersedes an earlier backup with the newer one', async () => {
    const place = await world();
    const first = await here(place, { 'p.txt': 'the first way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place);
    await reset(place, id);

    // It diverges again, and is reset again.
    const second = await here(place, { 'p.txt': 'the second way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork, again\n' });
    await reset(place, id);

    const refs = await place.repos.listRefs(place.repository);
    const kept = refs.find((ref) => ref.name === `refs/hub/replaced/${PATCH}`)?.oid;
    expect(kept).toBe(second);
    expect(kept).not.toBe(first);
  });
});

describe('what a reset does about rebuilding', () => {
  it.each([
    ['an active patch', {}, true],
    ['a patch in conflict', { status: 'conflict' }, true],
    ['a disabled patch', { status: 'disabled' }, false],
  ])('queues one for %s', async (_what, over, expected) => {
    const place = await world();
    await here(place, { 'p.txt': 'the hub way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place, over);

    const body = (await (await reset(place, id)).json()) as Record<string, unknown>;

    expect(body['rebuild_triggered']).toBe(expected);
    if (expected) {
      expect(String(body['rebuild_job_id'])).not.toHaveLength(0);
    } else {
      expect(body['rebuild_job_id']).toBeUndefined();
    }
  });

  it('queues none where the workspace asked for none', async () => {
    const place = await world({
      settings: { patch_branch_source: 'origin', auto_rebuild_after_sync: false },
    });
    await here(place, { 'p.txt': 'the hub way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place);

    const body = (await (await reset(place, id)).json()) as Record<string, unknown>;

    expect(body['rebuild_triggered']).toBe(false);
  });

  it('queues none where one is already under way', async () => {
    const place = await world();
    await here(place, { 'p.txt': 'the hub way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place);
    await place.service.store.run(
      `INSERT INTO rebuilds (id, workspace_slug, status, strategy, fail_mode, submitted_by, queued_at)
       VALUES ('already', ?, 'running', 'rebase', 'fail_fast', 'u-1', '2026-03-01T12:00:00Z')`,
      [place.slug],
    );

    const body = (await (await reset(place, id)).json()) as Record<string, unknown>;

    expect(body['rebuild_triggered']).toBe(false);
  });

  it('records how the patch now stands against the fork', async () => {
    const place = await world();
    await here(place, { 'p.txt': 'the hub way\n' }, undefined);
    const theirs = await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place);

    const body = (await (await reset(place, id)).json()) as Record<string, unknown>;

    expect(body['origin_sync_state']).toBe('in_sync');
    expect(body['origin_sha']).toBe(theirs);
    expect(body['origin_synced_at']).not.toBeUndefined();
  });
});

describe('a reset that cannot be made', () => {
  it.each([
    ['an archived workspace', { status: 'archived' as const }, 400],
    ['a standard workspace', { workspace_mode: 'standard' as const }, 400],
    ['a workspace that is not ready', { status: 'pending' as const }, 409],
  ])('is refused for %s', async (_what, over, expected) => {
    const place = await world(over);
    const id = await patchId(place);

    expect((await reset(place, id)).status).toBe(expected);
  });

  it('answers 404 for a patch nobody has', async () => {
    const place = await world();

    expect((await reset(place, 'nobody')).status).toBe(404);
  });

  it.each([
    ['a patch the upstream took', { status: 'merged_upstream' }],
    ['a patch that was removed', { status: 'deleted' }],
  ])('answers 409 for %s', async (_what, over) => {
    const place = await world();
    const id = await patchId(place, over);

    expect((await reset(place, id)).status).toBe(409);
  });

  it('answers 409 missing_on_origin where the fork has no such branch', async () => {
    const place = await world();
    await here(place, { 'p.txt': 'only here\n' }, undefined);
    const id = await patchId(place);

    const response = await reset(place, id);

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'missing_on_origin',
    );
    // The patch still describes where it really is.
    expect(await branchHere(place)).not.toBeUndefined();
  });

  it('answers 502 origin_fetch_failed where the fork cannot be read', async () => {
    const place = await world({}, { remote: fakeRemote({ fails: 500 }) });
    const id = await patchId(place);

    const response = await reset(place, id);

    expect(response.status).toBe(502);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'origin_fetch_failed',
    );
  });

  it('answers 409 workspace_busy while another operation holds the workspace', async () => {
    const place = await world();
    await onFork(place, { 'p.txt': 'from the fork\n' });
    const id = await patchId(place);
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await reset(place, id);
    await held?.release();

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
  });

  it('answers 409 ref_changed where the branch would not move', async () => {
    const place = await world();
    await here(place, { 'p.txt': 'the hub way\n' }, undefined);
    await onFork(place, { 'p.txt': 'the fork way\n' });
    const id = await patchId(place);
    const original = place.repos.receivePack.bind(place.repos);
    place.repos.receivePack = (() =>
      Promise.reject(new Error('another operation moved it'))) as typeof place.repos.receivePack;

    const response = await reset(place, id);
    place.repos.receivePack = original;

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'ref_changed',
    );
    // Nothing was recorded about a move that did not happen.
    const row = await place.service.store.all<{ origin_sha: string | null }>(
      'SELECT origin_sha FROM patches WHERE id = ?',
      [id],
    );
    expect(row[0]?.origin_sha).toBeNull();
  });
});
