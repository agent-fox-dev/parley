import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { FLUSH, concatBytes, encodePkt, encodeSpecial } from '../git/pktline.js';
import { ZERO_OID } from '../git/transport.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedPatch, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What a push to a patch branch sets in motion. The push itself is
 * package 07's; what is new is that the stack it belongs to is now
 * out of date, so the workspace rebuilds — once, however many things
 * ask for it.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  repos: RepositoryStore;
  slug: string;
  org: string;
  repository: string;
  credential: string;
  base: string;
}

async function world(settings: Record<string, unknown> = {}): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-trigger-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, jobs: 'store' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    settings: { auto_rebuild_after_push: true, ...settings },
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);
  await seedPatch(service.store, workspace.slug, 'a-patch');

  return {
    service,
    repos,
    slug: workspace.slug,
    org: org.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

/** A push of one ref, as a git client sends it. */
async function push(place: World, branch: string, to: string): Promise<Response> {
  return place.service.app.request(`/git/${place.org}/${place.slug}.git/git-receive-pack`, {
    method: 'POST',
    body: concatBytes([
      encodePkt(`${ZERO_OID} ${to} refs/heads/${branch}\u0000report-status\n`),
      encodeSpecial(FLUSH),
    ]),
    headers: {
      authorization: `Basic ${Buffer.from(`parley:${place.credential}`).toString('base64')}`,
    },
  });
}

async function queued(place: World): Promise<Array<{ submitted_by: string; status: string }>> {
  return place.service.store.all<{ submitted_by: string; status: string }>(
    'SELECT submitted_by, status FROM rebuilds WHERE workspace_slug = ?',
    [place.slug],
  );
}

describe('a push to a patch branch', () => {
  it('queues a rebuild, and the push does not wait for it', async () => {
    const place = await world();
    const moved = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'p.txt': 'the patch\n' },
      [place.base],
      'the patch',
    );

    const started = Date.now();
    const response = await push(place, 'a-patch', moved.oid);

    expect(response.status).toBe(200);
    // The report came back promptly: the rebuild is a job, not work
    // the client waits on.
    expect(Date.now() - started).toBeLessThan(2000);

    const jobs = await queued(place);
    expect(jobs).toHaveLength(1);
    expect(jobs[0]?.submitted_by).toBe('system:patch-push');
    expect(jobs[0]?.status).toBe('queued');
  });

  it('queues none for an ordinary branch', async () => {
    const place = await world();
    const moved = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'o.txt': 'ordinary\n' },
      [place.base],
      'ordinary',
    );

    await push(place, 'ordinary', moved.oid);

    expect(await queued(place)).toEqual([]);
  });

  it('queues none where the workspace asked for none', async () => {
    const place = await world({ auto_rebuild_after_push: false });
    const moved = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'p.txt': 'the patch\n' },
      [place.base],
      'the patch',
    );

    await push(place, 'a-patch', moved.oid);

    expect(await queued(place)).toEqual([]);
  });
});

describe('how many rebuilds a workspace may owe', () => {
  it('is one, whatever asks, until it has run', async () => {
    const place = await world();
    const first = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'p.txt': 'one\n' },
      [place.base],
      'one',
    );
    await push(place, 'a-patch', first.oid);
    expect(await queued(place)).toHaveLength(1);

    // A second push, and a reset, while the first is still queued.
    const second = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'p.txt': 'two\n' },
      [place.base],
      'two',
    );
    await push(place, 'a-patch', second.oid);

    expect(await queued(place)).toHaveLength(1);

    // Once it has run, the next push owes another.
    await place.service.store.run(
      "UPDATE rebuilds SET status = 'completed' WHERE workspace_slug = ?",
      [place.slug],
    );
    const third = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'shared\n', 'p.txt': 'three\n' },
      [place.base],
      'three',
    );
    await push(place, 'a-patch', third.oid);

    expect(await queued(place)).toHaveLength(2);
  });
});
