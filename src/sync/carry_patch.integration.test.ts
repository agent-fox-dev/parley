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
import { seedApiKey, seedOrg, seedPatch, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The carry-patch sync, which is a different operation from the
 * standard one rather than that one with extras. Its steps are
 * numbered in the reference, so the suite goes after the order as
 * much as the outcomes: a failure part way has to leave exactly what
 * the earlier steps reached and nothing of the later ones.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token';

interface World {
  service: Service;
  upstream: FakeRemote;
  fork: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  credential: string;
  base: string;
}

async function world(
  settings: Record<string, unknown> = {},
  options: { fork?: FakeRemote; upstream?: FakeRemote; credential?: boolean } = {},
): Promise<World> {
  const upstream = options.upstream ?? fakeRemote();
  const fork = options.fork ?? fakeRemote();
  cleanups.push(upstream.dispose, fork.dispose);
  await Promise.all([upstream.ready, fork.ready]);

  const dir = mkdtempSync(join(tmpdir(), 'parley-carry-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);

  // One fetch reaches whichever remote the URL names.
  const reach = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    return url.includes('/upstream.git') ? upstream.fetch(url, init) : fork.fetch(url, init);
  }) as unknown as typeof fetch;

  const service = await identityService([], { repos, fetch: reach, jobs: 'store' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    branch: 'main',
    git_url: `${fork.url}/fork.git`,
    upstream_url: `${upstream.url}/upstream.git`,
    settings: { auto_rebuild_after_sync: true, ...settings },
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);
  // Upstream starts where the hub does.
  const onUpstream = await buildCommit(
    upstream.store,
    upstream.repository,
    { 'base.txt': 'shared\n' },
    [],
    'base',
  );
  await upstream.store.updateRefs(upstream.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: onUpstream.oid, force: true },
  ]);
  await service.store.run('UPDATE workspaces SET upstream_head_sha = ? WHERE slug = ?', [
    onUpstream.oid,
    workspace.slug,
  ]);

  if (options.credential !== false) {
    await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);
  }

  return {
    service,
    upstream,
    fork,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

async function sync(place: World): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/sync`, {
    method: 'POST',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

/** Moves the upstream on, so there is something to absorb. */
async function advanceUpstream(place: World, files: Record<string, string>): Promise<string> {
  const head = (await place.upstream.store.listRefs(place.upstream.repository)).find(
    (ref) => ref.name === 'refs/heads/main',
  )?.oid;
  const built = await buildCommit(
    place.upstream.store,
    place.upstream.repository,
    { 'base.txt': 'shared\n', ...files },
    head === undefined ? [] : [head],
    'upstream moved',
  );
  await place.upstream.store.updateRefs(place.upstream.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

describe('which remotes a carry-patch sync reaches', () => {
  it('fetches upstream and leaves the fork alone where the patches are the hub’s', async () => {
    const place = await world({ patch_branch_source: 'hub' });
    await advanceUpstream(place, { 'new.txt': 'upstream work\n' });
    const forkAsked = place.fork.requests();

    const response = await sync(place);
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(body['origin_fetched']).toBe(false);
    expect(place.fork.requests()).toBe(forkAsked);
    expect(place.upstream.requests()).toBeGreaterThan(0);
    // The fields that belong to the fork are not offered.
    expect(body['patches_synced']).toBeUndefined();
    expect(body['patches_diverged']).toBeUndefined();
  });

  it('fetches both where the patches come from the fork', async () => {
    const place = await world({ patch_branch_source: 'origin' });
    await advanceUpstream(place, { 'new.txt': 'upstream work\n' });

    const body = (await (await sync(place)).json()) as Record<string, unknown>;

    expect(body['origin_fetched']).toBe(true);
    expect(place.fork.requests()).toBeGreaterThan(0);
    expect(body['patches_synced']).toEqual([]);
    expect(body['patches_diverged']).toEqual([]);
  });
});

describe('the order a carry-patch sync works in', () => {
  it('asks the fork for nothing when the credential cannot be resolved', async () => {
    const place = await world({ patch_branch_source: 'origin' }, { credential: false });

    const response = await sync(place);

    expect(response.status).toBe(502);
    expect(place.upstream.requests()).toBe(0);
    expect(place.fork.requests()).toBe(0);
  });

  it('never reaches the fork when upstream cannot be fetched', async () => {
    const place = await world(
      { patch_branch_source: 'origin' },
      { upstream: fakeRemote({ fails: 500 }) },
    );

    const response = await sync(place);

    expect(response.status).toBe(502);
    expect(place.fork.requests()).toBe(0);
  });

  it('answers origin_fetch_failed once upstream is in and the fork is not', async () => {
    const place = await world(
      { patch_branch_source: 'origin' },
      { fork: fakeRemote({ fails: 500 }) },
    );
    await advanceUpstream(place, { 'new.txt': 'upstream work\n' });

    const response = await sync(place);

    expect(response.status).toBe(502);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'origin_fetch_failed',
    );
    expect(place.upstream.requests()).toBeGreaterThan(0);
  });
});

describe('an upstream that was rewritten', () => {
  it('is reported rather than refused, and the head follows it', async () => {
    const place = await world({ patch_branch_source: 'hub' });
    // A history of its own, which is what a force-push leaves.
    const rewritten = await buildCommit(
      place.upstream.store,
      place.upstream.repository,
      { 'base.txt': 'upstream went another way\n' },
      [],
      'rewritten',
    );
    await place.upstream.store.updateRefs(place.upstream.repository, [
      { name: 'refs/heads/main', oldOid: null, newOid: rewritten.oid, force: true },
    ]);

    const response = await sync(place);
    const body = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(body['force_push_detected']).toBe(true);
    expect(body['upstream_head_sha']).toBe(rewritten.oid);
  });
});

describe('what a completed carry-patch sync answers with', () => {
  it('carries its own fields beside the workspace, and uses no sync_error', async () => {
    const place = await world({ patch_branch_source: 'hub' });

    const body = (await (await sync(place)).json()) as Record<string, unknown>;

    expect(body['slug']).toBe(place.slug);
    expect(body['patches_merged']).toEqual([]);
    expect(body['rebuild_triggered']).toBe(false);
    expect(body['rebuild_job_id']).toBeUndefined();
    expect(body['force_push_detected']).toBe(false);
    expect(body['origin_fetched']).toBe(false);
    expect(body['sync_error']).toBeNull();
    // Even a sync that found nothing says when it looked.
    expect(body['last_sync_at']).not.toBeNull();
  });
});

/** A branch in the hub's copy, and optionally a different one on the fork. */
async function patchBranch(
  place: World,
  branch: string,
  options: {
    here?: Record<string, string>;
    onFork?: Record<string, string>;
    from?: string;
    /** The same message both sides, where the two are meant to agree. */
    message?: string;
  } = {},
): Promise<{ here?: string; onFork?: string }> {
  const made: { here?: string; onFork?: string } = {};
  if (options.here !== undefined) {
    const built = await buildCommit(
      place.repos,
      place.repository,
      options.here,
      options.from === undefined ? [] : [options.from],
      options.message ?? `hub ${branch}`,
    );
    await place.repos.updateRefs(place.repository, [
      { name: `refs/heads/${branch}`, oldOid: null, newOid: built.oid, force: true },
    ]);
    made.here = built.oid;
  }
  if (options.onFork !== undefined) {
    const built = await buildCommit(
      place.fork.store,
      place.fork.repository,
      options.onFork,
      [],
      options.message ?? `fork ${branch}`,
    );
    await place.fork.store.updateRefs(place.fork.repository, [
      { name: `refs/heads/${branch}`, oldOid: null, newOid: built.oid, force: true },
    ]);
    made.onFork = built.oid;
  }
  await seedPatch(place.service.store, place.slug, branch);
  return made;
}

describe('refreshing the patch branches from the fork', () => {
  it('applies the first rule that fits to each branch', async () => {
    const place = await world({
      patch_branch_source: 'origin',
      patch_divergence_policy: 'replace',
    });
    await patchBranch(place, 'only-here', { here: { 'a.txt': 'only the hub\n' } });
    await patchBranch(place, 'only-there', { onFork: { 'b.txt': 'only the fork\n' } });
    await patchBranch(place, 'agreed', {
      here: { 'c.txt': 'the same\n' },
      onFork: { 'c.txt': 'the same\n' },
      message: 'the same commit',
    });
    const diverged = await patchBranch(place, 'diverged', {
      here: { 'd.txt': 'the hub way\n' },
      onFork: { 'd.txt': 'the fork way\n' },
    });

    const body = (await (await sync(place)).json()) as Record<string, unknown>;
    const synced = body['patches_synced'] as Array<Record<string, unknown>>;
    const byBranch = new Map(synced.map((one) => [one['branch_name'], one]));

    expect(byBranch.get('only-here')?.['action']).toBe('missing_on_origin');
    expect(byBranch.get('only-there')?.['action']).toBe('created');
    expect(byBranch.get('agreed')?.['action']).toBe('none');
    expect(byBranch.get('diverged')?.['action']).toBe('replaced');
    expect(byBranch.get('diverged')?.['replaced_sha']).toBe(diverged.here);
    expect(body['patches_diverged']).toEqual([]);

    // The tip the hub dropped is still recoverable.
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.find((ref) => ref.name === 'refs/hub/replaced/diverged')?.oid).toBe(diverged.here);

    // One event for the sync, naming the branches in each state, and
    // one for the branch whose tip it discarded.
    const [recorded] = await recordedEvents(place.service.store, 'hub.patch.sync');
    expect(recorded?.metadata).toEqual({
      origin_fetched: true,
      created: ['only-there'],
      fast_forwarded: [],
      replaced: ['diverged'],
      diverged: [],
      missing_on_origin: ['only-here'],
    });
    const [replaced] = await recordedEvents(place.service.store, 'hub.patch.replace');
    expect(replaced?.metadata).toMatchObject({
      branch_name: 'diverged',
      replaced_sha: diverged.here,
    });
    // A sync is not a reset, and says so by setting no trigger.
    expect(replaced?.metadata['trigger']).toBeUndefined();
  });

  it('leaves a diverged branch alone and names it under the report policy', async () => {
    const place = await world({
      patch_branch_source: 'origin',
      patch_divergence_policy: 'report',
    });
    const made = await patchBranch(place, 'diverged', {
      here: { 'd.txt': 'the hub way\n' },
      onFork: { 'd.txt': 'the fork way\n' },
    });

    const body = (await (await sync(place)).json()) as Record<string, unknown>;

    expect(body['patches_diverged']).toEqual(['diverged']);
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.find((ref) => ref.name === 'refs/heads/diverged')?.oid).toBe(made.here);
  });

  it('never refreshes the integration branch or a patch the upstream took', async () => {
    const place = await world({ patch_branch_source: 'origin' });
    await patchBranch(place, 'integration', { onFork: { 'i.txt': 'the fork integration\n' } });
    await patchBranch(place, 'landed', { onFork: { 'l.txt': 'the fork copy\n' } });
    await place.service.store.run('UPDATE patches SET status = ? WHERE branch_name = ?', [
      'merged_upstream',
      'landed',
    ]);

    const body = (await (await sync(place)).json()) as Record<string, unknown>;
    const synced = body['patches_synced'] as Array<Record<string, unknown>>;

    expect(synced.map((one) => one['branch_name'])).not.toContain('integration');
    expect(synced.map((one) => one['branch_name'])).not.toContain('landed');
  });

  it('records how each patch stands, and clears it all where the hub owns them', async () => {
    const place = await world({ patch_branch_source: 'origin' });
    const made = await patchBranch(place, 'tracked', {
      here: { 't.txt': 'the same\n' },
      onFork: { 't.txt': 'the same\n' },
      message: 'the same commit',
    });

    await sync(place);
    const recorded = await place.service.store.all<{
      origin_sync_state: string | null;
      origin_sha: string | null;
    }>('SELECT origin_sync_state, origin_sha FROM patches WHERE branch_name = ?', ['tracked']);
    expect(recorded[0]?.origin_sync_state).toBe('in_sync');
    expect(recorded[0]?.origin_sha).toBe(made.onFork);

    // The same workspace, told its patches are its own.
    await place.service.store.run('UPDATE workspaces SET settings = ? WHERE slug = ?', [
      JSON.stringify({ patch_branch_source: 'hub', auto_rebuild_after_sync: true }),
      place.slug,
    ]);
    await sync(place);

    const cleared = await place.service.store.all<{ origin_sync_state: string | null }>(
      'SELECT origin_sync_state FROM patches WHERE branch_name = ?',
      ['tracked'],
    );
    expect(cleared[0]?.origin_sync_state).toBeNull();
  });
});

describe('a branch refresh that fails', () => {
  it('ends the sync, keeps what it reached and writes no sync fields', async () => {
    const place = await world({ patch_branch_source: 'origin' });
    await patchBranch(place, 'first', { onFork: { 'f.txt': 'the fork copy\n' } });
    await patchBranch(place, 'second', { onFork: { 's.txt': 'the fork copy\n' } });
    const before = await place.service.store.all<{ last_sync_at: string | null }>(
      'SELECT last_sync_at FROM workspaces WHERE slug = ?',
      [place.slug],
    );

    // The second branch cannot be written.
    const original = place.repos.receivePack.bind(place.repos);
    let seen = 0;
    place.repos.receivePack = (async (name, pack, changes) => {
      if (changes.some((change) => change.name === 'refs/heads/second')) {
        seen += 1;
        throw new Error('another operation changed it');
      }
      return original(name, pack, changes);
    }) as typeof place.repos.receivePack;

    const response = await sync(place);
    place.repos.receivePack = original;

    expect(response.status).toBe(500);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      'failed to update patch branch second',
    );
    expect(seen).toBe(1);

    // The first branch did move, and says so.
    const refs = await place.repos.listRefs(place.repository);
    expect(refs.some((ref) => ref.name === 'refs/heads/first')).toBe(true);
    const recorded = await place.service.store.all<{ origin_sync_state: string | null }>(
      'SELECT origin_sync_state FROM patches WHERE branch_name = ?',
      ['first'],
    );
    expect(recorded[0]?.origin_sync_state).toBe('in_sync');

    // And the sync fields are untouched.
    const after = await place.service.store.all<{
      last_sync_at: string | null;
      upstream_head_sha: string | null;
    }>('SELECT last_sync_at, upstream_head_sha FROM workspaces WHERE slug = ?', [place.slug]);
    expect(after[0]?.last_sync_at).toBe(before[0]?.last_sync_at ?? null);

    // A rebuild is still owed for the branch that moved.
    const queued = await place.service.store.all<{ id: string }>(
      "SELECT id FROM rebuilds WHERE status = 'queued'",
      [],
    );
    expect(queued).toHaveLength(1);
  });
});

describe('what a carry-patch sync does about rebuilding', () => {
  it('queues one when the upstream advanced, and not when nothing did', async () => {
    const place = await world({ patch_branch_source: 'hub' });
    await patchBranch(place, 'one', { here: { 'o.txt': 'a patch\n' } });

    const quiet = (await (await sync(place)).json()) as Record<string, unknown>;
    expect(quiet['rebuild_triggered']).toBe(false);
    expect(quiet['rebuild_job_id']).toBeUndefined();

    await advanceUpstream(place, { 'new.txt': 'upstream work\n' });
    const moved = (await (await sync(place)).json()) as Record<string, unknown>;
    expect(moved['rebuild_triggered']).toBe(true);
    expect(String(moved['rebuild_job_id'])).not.toHaveLength(0);
  });

  it('queues none where the workspace asked for none, or one is under way', async () => {
    const off = await world({ patch_branch_source: 'hub', auto_rebuild_after_sync: false });
    await advanceUpstream(off, { 'new.txt': 'upstream work\n' });
    expect(((await (await sync(off)).json()) as Record<string, unknown>)['rebuild_triggered']).toBe(
      false,
    );

    const busy = await world({ patch_branch_source: 'hub' });
    await advanceUpstream(busy, { 'new.txt': 'upstream work\n' });
    await busy.service.store.run(
      `INSERT INTO rebuilds (id, workspace_slug, status, strategy, fail_mode, submitted_by, queued_at)
       VALUES ('already', ?, 'running', 'rebase', 'fail_fast', 'u-1', '2026-03-01T12:00:00Z')`,
      [busy.slug],
    );
    const answered = (await (await sync(busy)).json()) as Record<string, unknown>;
    expect(answered['rebuild_triggered']).toBe(false);
    expect(answered['rebuild_job_id']).toBeUndefined();
  });
});

/**
 * Three patches that each landed upstream in one of the three
 * shapes, and one that did not land at all.
 */
async function landed(detection: string) {
  const place = await world({
    patch_branch_source: 'hub',
    squash_merge_detection: detection,
  });

  // By ancestry: the patch's own commit is in the upstream's history.
  const byAncestry = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'shared\n', 'anc.txt': 'carried\n' },
    [place.base],
    'carried upstream as it was',
  );
  await place.repos.updateRefs(place.repository, [
    { name: 'refs/heads/by-ancestry', oldOid: null, newOid: byAncestry.oid, force: true },
  ]);
  await seedPatch(place.service.store, place.slug, 'by-ancestry');

  // By content: the upstream holds the same tree under another commit.
  const byContent = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'shared\n', 'squashed.txt': 'carried\n' },
    [place.base],
    'carried upstream, squashed',
  );
  await place.repos.updateRefs(place.repository, [
    { name: 'refs/heads/by-content', oldOid: null, newOid: byContent.oid, force: true },
  ]);
  await seedPatch(place.service.store, place.slug, 'by-content');

  // By pull request number, which is what a forge writes.
  const byNumber = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'shared\n', 'pr.txt': 'carried\n' },
    [place.base],
    'carried upstream through a pull request',
  );
  await place.repos.updateRefs(place.repository, [
    { name: 'refs/heads/by-number', oldOid: null, newOid: byNumber.oid, force: true },
  ]);
  await seedPatch(place.service.store, place.slug, 'by-number');
  await place.service.store.run('UPDATE patches SET upstream_pr_url = ? WHERE branch_name = ?', [
    'https://example.test/upstream/repo/pull/42',
    'by-number',
  ]);

  // One that is still the workspace's own.
  const own = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'shared\n', 'own.txt': 'still ours\n' },
    [place.base],
    'still ours',
  );
  await place.repos.updateRefs(place.repository, [
    { name: 'refs/heads/still-ours', oldOid: null, newOid: own.oid, force: true },
  ]);
  await seedPatch(place.service.store, place.slug, 'still-ours');

  // Upstream takes the first as it was, the second's content under
  // another commit, and the third with the number in its message.
  const head = (await place.upstream.store.listRefs(place.upstream.repository)).find(
    (ref) => ref.name === 'refs/heads/main',
  )?.oid;
  const carried = await buildCommit(
    place.upstream.store,
    place.upstream.repository,
    { 'base.txt': 'shared\n', 'anc.txt': 'carried\n' },
    [head ?? ''],
    'carried upstream as it was',
  );
  const squashed = await buildCommit(
    place.upstream.store,
    place.upstream.repository,
    { 'base.txt': 'shared\n', 'squashed.txt': 'carried\n' },
    [carried.oid],
    'somebody else squashed it',
  );
  const numbered = await buildCommit(
    place.upstream.store,
    place.upstream.repository,
    { 'base.txt': 'shared\n', 'pr.txt': 'carried\n', 'extra.txt': 'and more\n' },
    [squashed.oid],
    'take the patch (#42)',
  );
  await place.upstream.store.updateRefs(place.upstream.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: numbered.oid, force: true },
  ]);

  const body = (await (await sync(place)).json()) as Record<string, unknown>;
  return { place, merged: body['patches_merged'] as string[] };
}

describe('finding the patches the upstream has taken', () => {
  it('matches the ancestry alone where that is all it is asked for', async () => {
    const { merged } = await landed('ancestry_only');

    expect(merged).toEqual(['by-ancestry']);
  });

  it('matches content and a pull request number where it is asked for those', async () => {
    const { merged } = await landed('content_based');

    expect(merged).toContain('by-content');
    expect(merged).toContain('by-number');
  });

  it('matches all three where it is asked for both, and never the one still carried', async () => {
    const { place, merged } = await landed('both');

    expect(merged.toSorted()).toEqual(['by-ancestry', 'by-content', 'by-number']);
    expect(merged).not.toContain('still-ours');

    // Each matched patch is marked, and keeps nothing about the fork.
    const marked = await place.service.store.all<{
      branch_name: string;
      status: string;
      origin_sha: string | null;
    }>('SELECT branch_name, status, origin_sha FROM patches WHERE workspace_slug = ?', [
      place.slug,
    ]);
    for (const row of marked) {
      expect(row.status).toBe(row.branch_name === 'still-ours' ? 'active' : 'merged_upstream');
      expect(row.origin_sha).toBeNull();
    }
  });
});
