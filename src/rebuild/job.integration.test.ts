import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { newIdentifier } from '../auth/credentials.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import { storeLocks } from '../platform/store_lock.js';
import { workspaceScope } from '../platform/secrets.js';
import type { SecretStore } from '../platform/secrets.js';
import { deploymentKey, sealedSecrets } from '../secrets/store.js';
import { repositoryOf } from '../repo/repository.js';
import { conflictIdentity } from '../history/conflict.js';
import { resolutionStore } from '../history/resolutions.js';
import { livePatches } from '../patch/list.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { SECRET_KEY } from '../testutil/app.js';
import { SEED_NOW, freshStore, seedClock, seedPatch, seedWorkspace } from '../testutil/identity.js';
import { insertRebuild, rebuildById } from './record.js';
import { type RebuildDeps, runRebuild } from './job.js';

/**
 * The rebuild, run against a real repository and a real history
 * engine. What it leaves behind — the branch, the patch list, the
 * per-patch results — is read back every time, because a job that
 * reports success and leaves the stack wrong is the failure this
 * package exists to avoid.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-the-fork';

interface World {
  store: RelationalStore;
  repos: RepositoryStore;
  secrets: SecretStore;
  locks: Locks;
  clock: Clock;
  remote: FakeRemote;
  slug: string;
  repository: string;
  deps: RebuildDeps;
  /** Where upstream stands, which is what a rebuild starts from. */
  upstream: string;
}

/**
 * A carry-patch workspace whose repository holds an upstream branch
 * and whatever patch branches the suite asks for.
 */
async function world(
  branches: Record<string, { files: Record<string, string>; from?: 'upstream' }> = {},
  over: object = {},
  options: { remote?: FakeRemote } = {},
): Promise<World> {
  const remote = options.remote ?? fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-rebuild-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  const repos = localRepositoryStore(dir);
  const clock = seedClock();
  const key = await deploymentKey(SECRET_KEY);
  const secrets = sealedSecrets(seeded.store, key as CryptoKey);
  const locks = storeLocks(seeded.store, clock);

  const workspace = await seedWorkspace(seeded.store, {
    owner_id: 'u-1',
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    branch: 'main',
    git_url: `${remote.url}/fork.git`,
    upstream_url: `${remote.url}/upstream.git`,
    ...over,
  });
  const repository = repositoryOf(workspace);
  await repos.create(repository);

  // Upstream, in the hub's copy and on the remote the rebuild reads.
  const base = await buildCommit(repos, repository, { 'base.txt': 'from upstream\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);
  const onRemote = await buildCommit(
    remote.store,
    remote.repository,
    { 'base.txt': 'from upstream\n' },
    [],
    'base',
  );
  await remote.store.updateRefs(remote.repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: onRemote.oid, force: true },
  ]);

  for (const [branch, what] of Object.entries(branches)) {
    const built = await buildCommit(
      repos,
      repository,
      { 'base.txt': 'from upstream\n', ...what.files },
      [base.oid],
      `on ${branch}`,
    );
    await repos.updateRefs(repository, [
      { name: `refs/heads/${branch}`, oldOid: null, newOid: built.oid, force: true },
    ]);
  }

  await secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);

  const deps: RebuildDeps = {
    store: seeded.store,
    repos,
    locks,
    clock,
    secrets,
    fetch: remote.fetch,
  };
  return {
    store: seeded.store,
    repos,
    secrets,
    locks,
    clock,
    remote,
    slug: workspace.slug,
    repository,
    deps,
    upstream: base.oid,
  };
}

/** Queues a rebuild and runs it, answering the record it left. */
async function rebuild(
  place: World,
  over: { strategy?: 'rebase' | 'merge'; fail_mode?: 'fail_fast' | 'continue' } = {},
) {
  const id = newIdentifier();
  await insertRebuild(
    place.store,
    {
      id,
      workspace_slug: place.slug,
      strategy: over.strategy ?? 'rebase',
      fail_mode: over.fail_mode ?? 'fail_fast',
      submitted_by: 'u-1',
    },
    SEED_NOW.toISOString(),
  );
  return runRebuild(place.deps, id);
}

async function branchAt(place: World, branch: string): Promise<string | undefined> {
  return (await place.repos.listRefs(place.repository)).find(
    (ref) => ref.name === `refs/heads/${branch}`,
  )?.oid;
}

describe('a rebuild of a patch stack', () => {
  it('starts from the upstream head and applies the patches in order', async () => {
    const place = await world({
      'patch-one': { files: { 'one.txt': 'first patch\n' } },
      'patch-two': { files: { 'two.txt': 'second patch\n' } },
    });
    await seedPatch(place.store, place.slug, 'patch-one');
    await seedPatch(place.store, place.slug, 'patch-two');

    const job = await rebuild(place);

    expect(job.status).toBe('completed');
    expect(job.upstream_head).toBe(place.upstream);
    expect(job.results.map((one) => one.branch_name)).toEqual(['patch-one', 'patch-two']);
    expect(job.results.every((one) => one.outcome === 'applied')).toBe(true);
    expect(await branchAt(place, 'integration')).toBe(job.new_integration_head);
  });

  it('skips the patches it must, each with its reason', async () => {
    const place = await world({
      ordinary: { files: { 'o.txt': 'ordinary\n' } },
      'also-ordinary': { files: { 'p.txt': 'also\n' } },
    });
    await seedPatch(place.store, place.slug, 'merged', { status: 'merged_upstream' });
    await seedPatch(place.store, place.slug, 'ordinary');
    await seedPatch(place.store, place.slug, 'switched-off', { status: 'disabled' });
    await seedPatch(place.store, place.slug, 'nowhere');
    await seedPatch(place.store, place.slug, 'also-ordinary');

    const job = await rebuild(place);

    const reasons = new Map(job.results.map((one) => [one.branch_name, one.reason]));
    expect(reasons.get('merged')).toBe('merged_upstream');
    expect(reasons.get('switched-off')).toBe('disabled');
    expect(reasons.get('nowhere')).toBe('branch_not_found');
    expect(
      job.results.filter((one) => one.outcome === 'applied').map((one) => one.branch_name),
    ).toEqual(['ordinary', 'also-ordinary']);
  });

  it('sets the branch, retires what upstream took and closes the positions', async () => {
    const place = await world({
      one: { files: { 'one.txt': 'first\n' } },
      two: { files: { 'two.txt': 'second\n' } },
      three: { files: { 'three.txt': 'third\n' } },
    });
    await seedPatch(place.store, place.slug, 'one');
    await seedPatch(place.store, place.slug, 'landed', { status: 'merged_upstream' });
    await seedPatch(place.store, place.slug, 'two');
    await seedPatch(place.store, place.slug, 'three');

    const job = await rebuild(place);

    expect(job.status).toBe('completed');
    expect(job.previous_integration_head).toBeNull();
    expect(job.new_integration_head).toBe(await branchAt(place, 'integration'));

    const left = await livePatches(place.store, place.slug);
    expect(left.map((patch) => patch.branch_name)).toEqual(['one', 'two', 'three']);
    expect(left.map((patch) => patch.position)).toEqual([1, 2, 3]);
    const retired = await place.store.all<{ status: string; deleted_at: string | null }>(
      'SELECT status, deleted_at FROM patches WHERE branch_name = ?',
      ['landed'],
    );
    expect(retired[0]?.status).toBe('deleted');
    expect(retired[0]?.deleted_at).not.toBeNull();
  });
});

/** Two patches that touch the same line, so the second conflicts. */
async function conflicting(mode: 'fail_fast' | 'continue') {
  const place = await world();
  const base = (await branchAt(place, 'main')) ?? '';
  for (const [branch, line] of [
    ['first', 'one side\n'],
    ['second', 'the other side\n'],
  ] as const) {
    const built = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': line },
      [base],
      `on ${branch}`,
    );
    await place.repos.updateRefs(place.repository, [
      { name: `refs/heads/${branch}`, oldOid: null, newOid: built.oid, force: true },
    ]);
  }
  const third = await buildCommit(
    place.repos,
    place.repository,
    { 'base.txt': 'from upstream\n', 'third.txt': 'untouched\n' },
    [base],
    'on third',
  );
  await place.repos.updateRefs(place.repository, [
    { name: 'refs/heads/third', oldOid: null, newOid: third.oid, force: true },
  ]);

  await seedPatch(place.store, place.slug, 'first');
  await seedPatch(place.store, place.slug, 'second');
  await seedPatch(place.store, place.slug, 'third');
  return { place, job: await rebuild(place, { fail_mode: mode }) };
}

describe('a patch that will not apply', () => {
  it('ends the job under fail_fast, naming the patch and its files', async () => {
    const { job } = await conflicting('fail_fast');

    expect(job.status).toBe('failed');
    const conflicted = job.results.find((one) => one.outcome === 'conflict');
    expect(conflicted?.branch_name).toBe('second');
    expect(conflicted?.conflict_files).toEqual(['base.txt']);
    // Nothing after it was attempted.
    expect(job.results.map((one) => one.branch_name)).toEqual(['first', 'second']);
  });

  it('marks the patch and carries on under continue', async () => {
    const { place, job } = await conflicting('continue');

    expect(job.status).toBe('completed');
    expect(job.results.map((one) => one.outcome)).toEqual(['applied', 'conflict', 'applied']);

    const marked = await place.store.all<{ status: string }>(
      'SELECT status FROM patches WHERE branch_name = ?',
      ['second'],
    );
    expect(marked[0]?.status).toBe('conflict');
  });

  it('applies cleanly once the conflict has been resolved by hand', async () => {
    const { place } = await conflicting('continue');
    const conflict = {
      path: 'base.txt',
      base: 'from upstream\n',
      ours: 'one side\n',
      theirs: 'the other side\n',
    };
    await resolutionStore(place.store, place.clock).record(
      place.repository,
      conflict,
      'both sides, settled by hand\n',
    );
    expect(await conflictIdentity(conflict)).toMatch(/^[0-9a-f]{64}$/);
    await place.store.run('UPDATE patches SET status = ? WHERE branch_name = ?', [
      'active',
      'second',
    ]);

    const again = await rebuild(place, { fail_mode: 'fail_fast' });

    expect(again.status).toBe('completed');
    expect(again.results.map((one) => one.outcome)).toEqual(['applied', 'applied', 'applied']);
  });
});

/**
 * Runs something the first time the job writes the head it is
 * building, which is in the middle of the run: after the start has
 * let the lock go and before the end takes it again.
 */
function duringTheRun(place: World, what: () => Promise<void>): void {
  const original = place.repos.updateRefs.bind(place.repos);
  let done = false;
  place.repos.updateRefs = (async (name, changes) => {
    await original(name, changes);
    if (!done && changes.some((change) => change.name.startsWith('refs/hub/rebuild/'))) {
      done = true;
      await what();
    }
  }) as typeof place.repos.updateRefs;
}

describe('what else may happen while a rebuild runs', () => {
  it('leaves the lock free in the middle, and holds it at the start and the end', async () => {
    const place = await world({ one: { files: { 'one.txt': 'first\n' } } });
    await seedPatch(place.store, place.slug, 'one');

    let freeInTheMiddle = false;
    duringTheRun(place, async () => {
      const held = await place.locks.take(`workspace:${place.slug}`);
      freeInTheMiddle = held !== undefined;
      await held?.release();
    });

    const job = await rebuild(place);

    expect(job.status).toBe('completed');
    expect(freeInTheMiddle, 'the lock was free while patches were applied').toBe(true);

    // And it is taken at the start: a lock held before the job runs
    // stops it getting that far.
    const blocked = await place.locks.take(`workspace:${place.slug}`);
    await expect(rebuild(place)).rejects.toThrow(/another operation/);
    await blocked?.release();
  });

  it('shows the results of the patches finished so far while it runs', async () => {
    const place = await world({
      one: { files: { 'one.txt': 'first\n' } },
      two: { files: { 'two.txt': 'second\n' } },
    });
    await seedPatch(place.store, place.slug, 'one');
    await seedPatch(place.store, place.slug, 'two');

    const seen: Array<{ status: string; results: number }> = [];
    duringTheRun(place, async () => {
      const running = await place.store.all<{ id: string }>(
        'SELECT id FROM rebuilds WHERE workspace_slug = ?',
        [place.slug],
      );
      const read = await rebuildById(place.store, running[0]?.id ?? '');
      seen.push({ status: read?.status ?? '', results: read?.results.length ?? 0 });
    });

    const job = await rebuild(place);

    expect(seen[0]?.status, 'the job reads as running while it runs').toBe('running');
    expect(seen[0]?.results).toBeLessThan(job.results.length);
    expect(job.results).toHaveLength(2);
  });

  it('queues one follow-up, from the hub itself, when a tip moves under it', async () => {
    const place = await world({
      one: { files: { 'one.txt': 'first\n' } },
      two: { files: { 'two.txt': 'second\n' } },
    });
    await seedPatch(place.store, place.slug, 'one');
    await seedPatch(place.store, place.slug, 'two');

    duringTheRun(place, async () => {
      // Somebody pushes to a patch branch while the rebuild runs.
      const moved = await buildCommit(
        place.repos,
        place.repository,
        { 'base.txt': 'from upstream\n', 'one.txt': 'first, amended\n' },
        [place.upstream],
        'amended',
      );
      await place.store.run('SELECT 1', []);
      await (place.repos as unknown as { updateRefs: RepositoryStore['updateRefs'] }).updateRefs(
        place.repository,
        [{ name: 'refs/heads/one', oldOid: null, newOid: moved.oid, force: true }],
      );
    });

    const job = await rebuild(place);

    expect(job.status).toBe('completed');
    const queued = await place.store.all<{ submitted_by: string; follow_up_of: string }>(
      "SELECT submitted_by, follow_up_of FROM rebuilds WHERE status = 'queued'",
      [],
    );
    expect(queued).toHaveLength(1);
    expect(queued[0]?.submitted_by).toBe('system:stale-snapshot');
    expect(queued[0]?.follow_up_of).toBe(job.id);
  });
});

describe('the rebuilt branch and the fork', () => {
  it.each([
    ['a fork that takes it', {}, true],
    ['a fork that refuses it', { refusesPush: 'the fork says no' }, false],
  ])('pushes it where the setting says so: %s', async (_what, options, expected) => {
    const place = await world(
      { one: { files: { 'one.txt': 'first\n' } } },
      { settings: { rebuild_push_integration_branch: true } },
      { remote: fakeRemote({ requires: `Bearer ${TOKEN}`, ...options }) },
    );
    await seedPatch(place.store, place.slug, 'one');

    const job = await rebuild(place);

    // A fork that will not take it does not undo the rebuild.
    expect(job.error).toBeNull();
    expect(job.status).toBe('completed');
    expect(job.integration_branch_pushed).toBe(expected);
  });

  it('pushes nothing where the setting is off', async () => {
    const place = await world({ one: { files: { 'one.txt': 'first\n' } } });
    await seedPatch(place.store, place.slug, 'one');
    const before = place.remote.pushed().length;

    const job = await rebuild(place);

    expect(job.integration_branch_pushed).toBe(false);
    expect(place.remote.pushed()).toHaveLength(before);
  });
});
