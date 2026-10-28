import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { newIdentifier } from '../auth/credentials.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import { storeLocks } from '../platform/store_lock.js';
import { repositoryOf } from '../repo/repository.js';
import { readSnapshot } from '../testutil/history.js';
import { buildCommit } from '../testutil/history.js';
import { SEED_NOW, freshStore, seedClock, seedWorkspace } from '../testutil/identity.js';
import { sweepStalledMerges } from '../jobs/stall_sweep.js';
import { eventRecorder } from '../audit/recorder.js';
import { recordedEvents } from '../testutil/app.js';
import { insertMerge, mergeById } from './job.js';
import { type MergeRunDeps, runMerge } from './run.js';

/**
 * Running a merge against a real repository and the real history
 * engine. A merge is a rebase and a fast-forward, so what the target
 * holds afterwards is the thing to look at: the source's commits on
 * top of the target's, and no merge commit anywhere.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  store: RelationalStore;
  repos: RepositoryStore;
  locks: Locks;
  clock: Clock;
  slug: string;
  repository: string;
  deps: MergeRunDeps;
  base: string;
}

async function world(): Promise<World> {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-merge-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  const repos = localRepositoryStore(dir);
  const clock = seedClock();
  const locks = storeLocks(seeded.store, clock);
  const workspace = await seedWorkspace(seeded.store, { owner_id: 'u-1', status: 'ready' });
  const repository = repositoryOf(workspace);
  await repos.create(repository);

  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);

  return {
    store: seeded.store,
    repos,
    locks,
    clock,
    slug: workspace.slug,
    repository,
    deps: {
      store: seeded.store,
      repos,
      locks,
      clock,
      waitMs: 200,
      events: eventRecorder({ store: seeded.store, clock }),
    },
    base: base.oid,
  };
}

/** A branch off the shared base. */
async function branch(
  place: World,
  name: string,
  files: Record<string, string>,
  from?: string,
): Promise<string> {
  const built = await buildCommit(
    place.repos,
    place.repository,
    files,
    [from ?? place.base],
    `on ${name}`,
  );
  await place.repos.updateRefs(place.repository, [
    { name: `refs/heads/${name}`, oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

async function submit(place: World, target: string, source: string): Promise<string> {
  const id = newIdentifier();
  await insertMerge(
    place.store,
    {
      id,
      workspace_slug: place.slug,
      target_branch: target,
      source_ref: source,
      submitted_by: 'u-1',
    },
    SEED_NOW.toISOString(),
  );
  return id;
}

async function headOf(place: World, name: string): Promise<string | undefined> {
  return (await place.repos.listRefs(place.repository)).find(
    (ref) => ref.name === `refs/heads/${name}`,
  )?.oid;
}

describe('a merge that applies', () => {
  it("puts the source's commits on top of the target's, with no merge commit", async () => {
    const place = await world();
    // The target moved on since the source branched.
    const onMain = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'main.txt': 'the target moved\n' },
      [place.base],
      'on main',
    );
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/main', oldOid: null, newOid: onMain.oid, force: true },
    ]);
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'feature.txt': 'the work\n' });

    const id = await submit(place, 'main', 'feature');
    expect(await runMerge(place.deps, id)).toBe('done');
    const job = await mergeById(place.store, id);

    expect(job?.status).toBe('completed');
    expect(job?.base_sha).toBe(onMain.oid);
    expect(job?.merged_sha).toBe(await headOf(place, 'main'));
    expect(job?.conflict_files).toEqual([]);

    // Both sides' work is there, and the history is linear.
    expect(await readSnapshot(place.repos, place.repository, job?.merged_sha ?? '')).toEqual({
      'base.txt': 'shared\n',
      'main.txt': 'the target moved\n',
      'feature.txt': 'the work\n',
    });
    const merged = await place.repos.readObject(place.repository, job?.merged_sha ?? '');
    const text = new TextDecoder().decode(merged?.data ?? new Uint8Array());
    expect(text.split('\n').filter((line) => line.startsWith('parent '))).toHaveLength(1);

    // Nobody waited on this job, so its event is the only account of
    // how it went.
    const [recorded] = await recordedEvents(place.store, 'hub.merge.complete');
    expect(recorded?.actor_type).toBe('system');
    expect(recorded?.metadata).toEqual({ base_sha: onMain.oid, merged_sha: job?.merged_sha });
  });

  it('leaves base_sha and merged_sha unset until it has finished', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');

    const queued = await mergeById(place.store, id);
    expect(queued?.status).toBe('queued');
    expect(queued?.base_sha).toBeNull();
    expect(queued?.merged_sha).toBeNull();
    expect(queued?.conflict_files).toEqual([]);
    expect(queued?.retry_count).toBe(0);
    expect(queued?.submitted_by).toBe('u-1');

    await runMerge(place.deps, id);

    const done = await mergeById(place.store, id);
    expect(done?.status).toBe('completed');
    expect(done?.base_sha).not.toBeNull();
    expect(done?.merged_sha).not.toBeNull();
  });
});

describe('a merge that conflicts', () => {
  it('fails with the conflicting files, and the target is where it was', async () => {
    const place = await world();
    const onMain = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'the target wrote this\n' },
      [place.base],
      'on main',
    );
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/main', oldOid: null, newOid: onMain.oid, force: true },
    ]);
    await branch(place, 'feature', { 'base.txt': 'the source wrote this\n' });

    const id = await submit(place, 'main', 'feature');
    await runMerge(place.deps, id);
    const job = await mergeById(place.store, id);

    expect(job?.status).toBe('failed');
    expect(job?.conflict_files).toEqual(['base.txt']);
    expect(await headOf(place, 'main')).toBe(onMain.oid);

    const [recorded] = await recordedEvents(place.store, 'hub.merge.fail');
    expect(Object.keys(recorded?.metadata ?? {})).toEqual(['reason']);
  });

  it('fails plainly where a branch it names is not there', async () => {
    const place = await world();
    const id = await submit(place, 'main', 'nowhere');

    await runMerge(place.deps, id);
    const job = await mergeById(place.store, id);

    expect(job?.status).toBe('failed');
    expect(job?.error).toContain('nowhere');
    expect(await headOf(place, 'main')).toBe(place.base);
  });
});

describe('a merge whose workspace is busy', () => {
  it('waits its turn rather than being refused, and runs once the lock frees', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');

    const held = await place.locks.take(`workspace:${place.slug}`);
    expect(held, 'the suite took the lock').toBeDefined();

    // It gives up its turn rather than failing: the workspace is
    // busy, which is not the merge's fault.
    expect(await runMerge(place.deps, id)).toBe('again');
    expect((await mergeById(place.store, id))?.status).toBe('queued');

    await held?.release();

    expect(await runMerge(place.deps, id)).toBe('done');
    expect((await mergeById(place.store, id))?.status).toBe('completed');
  });

  it('never runs once it has been cancelled', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');
    await place.store.run("UPDATE merges SET status = 'cancelled' WHERE id = ?", [id]);

    await runMerge(place.deps, id);

    expect((await mergeById(place.store, id))?.status).toBe('cancelled');
    expect(await headOf(place, 'main')).toBe(place.base);
  });
});

describe('a job whose runner went away', () => {
  it('runs again when it is redelivered, rather than staying at running', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');

    // What a killed runner leaves behind: a record that says running.
    await place.store.run("UPDATE merges SET status = 'running' WHERE id = ?", [id]);

    expect(await runMerge(place.deps, id)).toBe('done');

    const job = await mergeById(place.store, id);
    expect(job?.status).toBe('completed');
    expect(await headOf(place, 'main')).toBe(job?.merged_sha);
  });

  it('is not run twice over once it has finished', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');
    await runMerge(place.deps, id);
    const first = await mergeById(place.store, id);

    // The queue redelivers it; nothing should move.
    await runMerge(place.deps, id);

    expect(await mergeById(place.store, id)).toEqual(first);
    expect(await headOf(place, 'main')).toBe(first?.merged_sha);
  });
});

describe('a job that stopped making progress', () => {
  it('is given up on, with its retry counted, and can be queued again', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');

    // Running, and silent for longer than it should be.
    const longAgo = new Date(SEED_NOW.getTime() - 20 * 60 * 1000).toISOString();
    await place.store.run(
      "UPDATE merges SET status = 'running', started_at = ?, updated_at = ? WHERE id = ?",
      [longAgo, longAgo, id],
    );

    expect(await sweepStalledMerges(place.store, place.clock)).toBe(1);

    const given = await mergeById(place.store, id);
    expect(given?.status).toBe('dead_letter');
    expect(given?.retry_count).toBe(1);
    expect(given?.error).toContain('progress');
  });

  it('leaves alone one that is still saying something', async () => {
    const place = await world();
    const id = await submit(place, 'main', 'feature');
    await place.store.run("UPDATE merges SET status = 'running', updated_at = ? WHERE id = ?", [
      SEED_NOW.toISOString(),
      id,
    ]);

    expect(await sweepStalledMerges(place.store, place.clock)).toBe(0);
    expect((await mergeById(place.store, id))?.status).toBe('running');
  });
});

describe('the target branch while a merge is interrupted', () => {
  it.each([
    [
      'before the rebase',
      (place: World) => {
        const original = place.repos.listRefs.bind(place.repos);
        place.repos.listRefs = (() =>
          Promise.reject(new Error('the store went away'))) as typeof place.repos.listRefs;
        return () => {
          place.repos.listRefs = original;
        };
      },
    ],
    [
      'between the rebase and the fast-forward',
      (place: World) => {
        const original = place.repos.updateRefs.bind(place.repos);
        place.repos.updateRefs = (async (name, changes) => {
          if (changes.some((change) => change.name === 'refs/heads/main')) {
            throw new Error('the store went away');
          }
          return original(name, changes);
        }) as typeof place.repos.updateRefs;
        return () => {
          place.repos.updateRefs = original;
        };
      },
    ],
  ])(
    'holds the head it had, when the merge is interrupted %s',
    async (_what, breaking: (place: World) => () => void) => {
      const place = await world();
      await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
      const id = await submit(place, 'main', 'feature');
      const before = await headOf(place, 'main');

      const mend = breaking(place);
      await runMerge(place.deps, id);
      mend();

      expect(await headOf(place, 'main')).toBe(before);
      expect((await mergeById(place.store, id))?.status).toBe('failed');
    },
  );

  it('reaches the merged result once a requeued job runs', async () => {
    const place = await world();
    await branch(place, 'feature', { 'base.txt': 'shared\n', 'f.txt': 'work\n' });
    const id = await submit(place, 'main', 'feature');
    const before = await headOf(place, 'main');

    const original = place.repos.updateRefs.bind(place.repos);
    let broken = true;
    place.repos.updateRefs = (async (name, changes) => {
      if (broken && changes.some((change) => change.name === 'refs/heads/main')) {
        throw new Error('the store went away');
      }
      return original(name, changes);
    }) as typeof place.repos.updateRefs;

    await runMerge(place.deps, id);
    expect(await headOf(place, 'main')).toBe(before);

    // Queued again, against a store that works.
    broken = false;
    await place.store.run("UPDATE merges SET status = 'queued' WHERE id = ?", [id]);
    await runMerge(place.deps, id);

    const job = await mergeById(place.store, id);
    expect(job?.status).toBe('completed');
    expect(await headOf(place, 'main')).toBe(job?.merged_sha);
  });
});
