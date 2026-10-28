import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { newIdentifier } from '../auth/credentials.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { resolutionStore } from '../history/resolutions.js';
import { repositoryOf } from '../repo/repository.js';
import { insertRebuild, updateRebuild } from '../rebuild/record.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedClock,
  seedOrg,
  seedPatch,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';

/**
 * The dashboard. Everything in it can be read elsewhere, so what is
 * worth testing is that it agrees with itself: the counts are derived
 * from the list it carries, and a reader should never have to doubt
 * one against the other.
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
  const dir = mkdtempSync(join(tmpdir(), 'parley-status-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    upstream_url: 'https://example.test/upstream.git',
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'shared\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/integration', oldOid: null, newOid: base.oid, force: true },
  ]);

  return {
    service,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

async function dashboard(place: World): Promise<Record<string, unknown>> {
  const response = await place.service.app.request(
    `/api/v1/workspaces/${place.slug}/patch-status`,
    { headers: { authorization: `Bearer ${place.credential}` } },
  );
  return (await response.json()) as Record<string, unknown>;
}

describe('the patch-status dashboard', () => {
  it('reports the workspace, the latest rebuild and every patch in order', async () => {
    const place = await world();
    for (const [branch, status] of [
      ['one', 'active'],
      ['two', 'conflict'],
      ['three', 'disabled'],
      ['gone', 'deleted'],
    ] as const) {
      await seedPatch(place.service.store, place.slug, branch, { status });
    }
    const patches = await place.service.store.all<{ id: string; branch_name: string }>(
      'SELECT id, branch_name FROM patches WHERE workspace_slug = ?',
      [place.slug],
    );
    const id = newIdentifier();
    await insertRebuild(
      place.service.store,
      {
        id,
        workspace_slug: place.slug,
        strategy: 'rebase',
        fail_mode: 'continue',
        submitted_by: 'u-1',
      },
      SEED_NOW.toISOString(),
    );
    await updateRebuild(place.service.store, id, {
      status: 'completed',
      results: [
        {
          patch_id: patches.find((one) => one.branch_name === 'one')?.id ?? '',
          branch_name: 'one',
          position: 1,
          outcome: 'applied',
        },
        {
          patch_id: patches.find((one) => one.branch_name === 'two')?.id ?? '',
          branch_name: 'two',
          position: 2,
          outcome: 'conflict',
          conflict_files: ['pkg/api.go'],
        },
      ],
    });

    const body = await dashboard(place);
    const listed = body['patches'] as Array<Record<string, unknown>>;

    expect(body['workspace_slug']).toBe(place.slug);
    expect(body['integration_head_sha']).toBe(place.base);
    expect(body['last_rebuild']).toEqual({ id, status: 'completed' });
    expect(listed.map((one) => one['branch_name'])).toEqual(['one', 'two', 'three']);
    expect(listed[0]?.['last_rebuild_result']).toBe('success');
    expect(listed[1]?.['last_rebuild_result']).toBe('conflict');
    expect(listed[1]?.['conflict_files']).toEqual(['pkg/api.go']);
    // Files are offered only where the result was a conflict.
    expect(listed[0]?.['conflict_files']).toBeUndefined();
    expect(listed[2]?.['last_rebuild_result']).toBeNull();
  });

  it('reports no rebuild where none has run', async () => {
    const place = await world();
    await seedPatch(place.service.store, place.slug, 'one');

    const body = await dashboard(place);

    expect(body['last_rebuild']).toBeNull();
    expect(
      (body['patches'] as Array<Record<string, unknown>>)[0]?.['last_rebuild_result'],
    ).toBeNull();
  });

  it('offers the fork fields only where the patches come from the fork', async () => {
    const hub = await world({ settings: { patch_branch_source: 'hub' } });
    await seedPatch(hub.service.store, hub.slug, 'one');
    await hub.service.store.run('UPDATE patches SET origin_sync_state = ? WHERE branch_name = ?', [
      'diverged',
      'one',
    ]);

    const hubBody = await dashboard(hub);
    expect(
      (hubBody['patches'] as Array<Record<string, unknown>>)[0]?.['origin_sync_state'],
    ).toBeUndefined();
    expect((hubBody['summary'] as Record<string, number>)['patches_diverged']).toBe(0);

    const fromFork = await world({ settings: { patch_branch_source: 'origin' } });
    await seedPatch(fromFork.service.store, fromFork.slug, 'one');
    await fromFork.service.store.run(
      'UPDATE patches SET origin_sync_state = ? WHERE branch_name = ?',
      ['diverged', 'one'],
    );

    const forkBody = await dashboard(fromFork);
    expect((forkBody['patches'] as Array<Record<string, unknown>>)[0]?.['origin_sync_state']).toBe(
      'diverged',
    );
    expect((forkBody['summary'] as Record<string, number>)['patches_diverged']).toBe(1);
  });

  it('refuses a workspace with no stack', async () => {
    const place = await world({ workspace_mode: 'standard' });

    const response = await place.service.app.request(
      `/api/v1/workspaces/${place.slug}/patch-status`,
      { headers: { authorization: `Bearer ${place.credential}` } },
    );

    expect(response.status).toBe(400);
  });
});

describe("the dashboard's counts", () => {
  it('agree with the patches it lists, for every mix of statuses', async () => {
    // Every shape of stack the counts have to add up for.
    const mixes: Array<Array<[string, string]>> = [
      [],
      [['a', 'active']],
      [
        ['a', 'active'],
        ['b', 'conflict'],
      ],
      [
        ['a', 'active'],
        ['b', 'active'],
        ['c', 'merged_upstream'],
        ['d', 'disabled'],
        ['e', 'deleted'],
      ],
      [
        ['a', 'disabled'],
        ['b', 'disabled'],
      ],
    ];

    for (const mix of mixes) {
      const place = await world({ settings: { patch_branch_source: 'origin' } });
      for (const [branch, status] of mix) {
        await seedPatch(place.service.store, place.slug, branch, { status });
      }
      await place.service.store.run(
        "UPDATE patches SET origin_sync_state = 'diverged' WHERE branch_name = 'a'",
        [],
      );

      const body = await dashboard(place);
      const listed = body['patches'] as Array<Record<string, unknown>>;
      const summary = body['summary'] as Record<string, number>;
      const live = mix.filter(([, status]) => status !== 'deleted');

      expect(summary['total_patches'], JSON.stringify(mix)).toBe(listed.length);
      expect(summary['total_patches']).toBe(live.length);
      expect(
        (summary['active'] ?? 0) +
          (summary['merged_upstream'] ?? 0) +
          (summary['conflict'] ?? 0) +
          (summary['disabled'] ?? 0),
      ).toBe(summary['total_patches']);
      expect(summary['patches_diverged']).toBe(
        listed.filter((one) => one['origin_sync_state'] === 'diverged').length,
      );
    }
  });

  it('counts the resolutions, and none where they cannot be read', async () => {
    const place = await world();
    await resolutionStore(place.service.store, seedClock()).record(
      place.repository,
      { path: 'a.txt', base: 'one\n', ours: 'two\n', theirs: 'three\n' },
      'settled\n',
    );

    expect(
      ((await dashboard(place))['summary'] as Record<string, number>)['total_rerere_resolutions'],
    ).toBe(1);

    const original = place.service.store.all.bind(place.service.store);
    place.service.store.all = (async (sql: string, params?: readonly unknown[]) => {
      if (sql.includes('resolutions')) {
        throw new Error('the resolutions cannot be read');
      }
      return original(sql, params as never);
    }) as typeof place.service.store.all;

    const body = await dashboard(place);
    place.service.store.all = original;

    expect((body['summary'] as Record<string, number>)['total_rerere_resolutions']).toBe(0);
  });
});
