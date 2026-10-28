import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { mergeById } from '../merge/job.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The merge routes. The submit's order is the notable part: the body
 * is checked before the workspace is looked up, which is deliberately
 * the opposite of the anti-enumeration rule everywhere else.
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
  repository: string;
  credential: string;
  base: string;
}

async function world(over: object = {}): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-merges-'));
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
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  for (const branch of ['main', 'feature']) {
    await repos.updateRefs(repository, [
      { name: `refs/heads/${branch}`, oldOid: null, newOid: base.oid, force: true },
    ]);
  }

  return {
    service,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

async function call(
  place: World,
  path: string,
  init: { method?: string; body?: unknown; slug?: string } = {},
): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${init.slug ?? place.slug}${path}`, {
    method: init.method ?? 'GET',
    headers: {
      authorization: `Bearer ${place.credential}`,
      ...(init.body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(init.body === undefined ? {} : { body: JSON.stringify(init.body) }),
  });
}

describe('submitting a merge', () => {
  it('queues it and answers with the job', async () => {
    const place = await world();

    const response = await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'main', source_ref: 'feature' },
    });
    const job = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(202);
    expect(job['status']).toBe('queued');
    expect(job['target_branch']).toBe('main');
    expect(job['source_ref']).toBe('feature');
    expect(job['base_sha']).toBeNull();
    expect(job['conflict_files']).toEqual([]);
    expect(String(job['submitted_by'])).not.toHaveLength(0);
  });

  it.each([
    ['a body that is not an object', 'merge please'],
    ['no target branch', { source_ref: 'feature' }],
    ['no source ref', { target_branch: 'main' }],
    ['a target that breaks git’s rules', { target_branch: '-x', source_ref: 'feature' }],
    ['a source that breaks them', { target_branch: 'main', source_ref: 'a..b' }],
  ])('refuses %s', async (_what, body) => {
    const place = await world();

    expect((await call(place, '/merges', { method: 'POST', body })).status).toBe(400);
  });

  it('checks the body before it looks the workspace up', async () => {
    const place = await world();

    // A workspace this caller cannot see, and a name nobody could use.
    const response = await call(place, '/merges', {
      method: 'POST',
      slug: 'no-such-workspace',
      body: { target_branch: '-x', source_ref: 'feature' },
    });

    expect(response.status).toBe(400);
  });

  it.each([
    ['a target nobody has', { target_branch: 'nowhere', source_ref: 'feature' }],
    ['a source nobody has', { target_branch: 'main', source_ref: 'nowhere' }],
  ])('refuses %s', async (_what, body) => {
    const place = await world();

    const response = await call(place, '/merges', { method: 'POST', body });

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      'nowhere',
    );
  });

  it('refuses an unready workspace', async () => {
    const place = await world({ status: 'pending' });

    const response = await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'main', source_ref: 'feature' },
    });

    expect(response.status).toBe(400);
  });

  it('refuses the same merge twice and leaves the first alone', async () => {
    const place = await world();
    const first = await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'main', source_ref: 'feature' },
    });
    const id = ((await first.json()) as { id: string }).id;

    const second = await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'main', source_ref: 'feature' },
    });

    expect(second.status).toBe(409);
    expect(((await second.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'duplicate_merge',
    );
    expect((await mergeById(place.service.store, id))?.status).toBe('queued');
  });

  it('allows the same source into a different target', async () => {
    const place = await world();
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/other', oldOid: null, newOid: place.base, force: true },
    ]);
    await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'main', source_ref: 'feature' },
    });

    const second = await call(place, '/merges', {
      method: 'POST',
      body: { target_branch: 'other', source_ref: 'feature' },
    });

    expect(second.status).toBe(202);
  });
});
