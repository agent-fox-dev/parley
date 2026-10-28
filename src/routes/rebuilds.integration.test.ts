import { mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedPatch, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The preview, against a real repository and the real engine. Its
 * whole promise is that it predicts without doing, so the suite reads
 * the repository's refs and its objects before and after.
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
  root: string;
  slug: string;
  repository: string;
  credential: string;
  base: string;
}

async function world(over: object = {}): Promise<World> {
  const root = mkdtempSync(join(tmpdir(), 'parley-preview-'));
  cleanups.push(() => {
    rmSync(root, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(root);
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
    branch: 'main',
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(
    repos,
    repository,
    { 'base.txt': 'one\ntwo\nthree\n' },
    [],
    'base',
  );
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
  ]);

  return {
    service,
    repos,
    root,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    base: base.oid,
  };
}

async function branch(place: World, name: string, files: Record<string, string>): Promise<void> {
  const built = await buildCommit(place.repos, place.repository, files, [place.base], `on ${name}`);
  await place.repos.updateRefs(place.repository, [
    { name: `refs/heads/${name}`, oldOid: null, newOid: built.oid, force: true },
  ]);
}

async function preview(place: World): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/rebuild/preview`, {
    method: 'POST',
    headers: { authorization: `Bearer ${place.credential}` },
  });
}

/** Everything the repository holds, so a preview can be shown to add none of it. */
function contents(place: World): { refs: string; objects: number } {
  const objects = readdirSync(join(place.root, place.repository, 'objects')).length;
  return { refs: '', objects };
}

describe('a preview of a rebuild', () => {
  it('predicts each patch, carries past a conflict, and writes nothing', async () => {
    const place = await world();
    await branch(place, 'clean', { 'base.txt': 'one\ntwo\nthree\n', 'c.txt': 'quiet\n' });
    await branch(place, 'clashing', { 'base.txt': 'ONE\ntwo\nthree\n' });
    await branch(place, 'also-clashing', { 'base.txt': 'another one\ntwo\nthree\n' });
    await branch(place, 'after', { 'base.txt': 'one\ntwo\nthree\n', 'a.txt': 'later\n' });
    for (const name of ['clean', 'clashing', 'also-clashing', 'after']) {
      await seedPatch(place.service.store, place.slug, name);
    }
    await seedPatch(place.service.store, place.slug, 'switched-off', { status: 'disabled' });
    await seedPatch(place.service.store, place.slug, 'landed', { status: 'merged_upstream' });
    await seedPatch(place.service.store, place.slug, 'nowhere');

    const before = contents(place);
    const refsBefore = await place.repos.listRefs(place.repository);

    const response = await preview(place);
    const predicted = ((await response.json()) as { patches: Array<Record<string, unknown>> })
      .patches;

    expect(response.status).toBe(200);
    const byBranch = new Map(predicted.map((one) => [one['branch_name'], one]));

    expect(byBranch.get('clean')?.['outcome']).toBe('would_succeed');
    expect(String(byBranch.get('clean')?.['tree'])).toMatch(/^[0-9a-f]{40}$/);
    expect(byBranch.get('also-clashing')?.['outcome']).toBe('would_conflict');
    expect(byBranch.get('also-clashing')?.['conflict_files']).toEqual(['base.txt']);
    // A predicted conflict does not stop the predictions after it.
    expect(byBranch.get('after')?.['outcome']).toBe('would_succeed');
    // A branch that is not there would not stop a rebuild either.
    expect(byBranch.get('nowhere')?.['outcome']).toBe('would_succeed');
    expect(byBranch.get('nowhere')?.['reason']).toBe('branch_not_found');
    // What is not attempted is not reported as attempted.
    expect(byBranch.has('switched-off')).toBe(false);
    expect(byBranch.has('landed')).toBe(false);

    // And the repository is exactly as it was.
    expect(contents(place)).toEqual(before);
    expect(await place.repos.listRefs(place.repository)).toEqual(refsBefore);
  });

  it('answers 400 for a workspace with no stack to predict', async () => {
    const place = await world({ workspace_mode: 'standard' });

    const response = await preview(place);

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_mode_mismatch',
    );
  });
});
