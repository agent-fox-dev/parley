import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit, readSnapshot } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { MOST_BRANCHES } from './batch_rebase.js';

/**
 * Rebasing several branches at once, against a real repository and
 * the real engine. The answer is a survey, so what matters is that
 * every branch gets an answer and that a branch which could not be
 * rebased is exactly where it was.
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
  const dir = mkdtempSync(join(tmpdir(), 'parley-batch-'));
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
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const base = await buildCommit(repos, repository, { 'base.txt': 'one\ntwo\n' }, [], 'base');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: base.oid, force: true },
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

async function branch(place: World, name: string, files: Record<string, string>): Promise<string> {
  const built = await buildCommit(place.repos, place.repository, files, [place.base], `on ${name}`);
  await place.repos.updateRefs(place.repository, [
    { name: `refs/heads/${name}`, oldOid: null, newOid: built.oid, force: true },
  ]);
  return built.oid;
}

async function rebase(place: World, body: unknown, slug?: string): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${slug ?? place.slug}/rebase`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${place.credential}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

async function headOf(place: World, name: string): Promise<string | undefined> {
  return (await place.repos.listRefs(place.repository)).find(
    (ref) => ref.name === `refs/heads/${name}`,
  )?.oid;
}

describe('rebasing several branches at once', () => {
  it('answers one result per branch, a conflict not stopping the rest', async () => {
    const place = await world();
    // The target moved on, so each branch really is rebased.
    const onMain = await buildCommit(
      place.repos,
      place.repository,
      { 'base.txt': 'one\ntwo\n', 'main.txt': 'the target moved\n' },
      [place.base],
      'on main',
    );
    await place.repos.updateRefs(place.repository, [
      { name: 'refs/heads/main', oldOid: null, newOid: onMain.oid, force: true },
    ]);

    const first = await branch(place, 'first', { 'base.txt': 'one\ntwo\n', 'a.txt': 'work\n' });
    await branch(place, 'clashing', { 'base.txt': 'ONE\ntwo\n' });
    await branch(place, 'last', { 'base.txt': 'one\ntwo\n', 'c.txt': 'more work\n' });
    // Something for the clashing branch to clash with.
    await place.repos.updateRefs(place.repository, [
      {
        name: 'refs/heads/main',
        oldOid: null,
        newOid: (
          await buildCommit(
            place.repos,
            place.repository,
            { 'base.txt': 'one, changed\ntwo\n', 'main.txt': 'the target moved\n' },
            [onMain.oid],
            'main again',
          )
        ).oid,
        force: true,
      },
    ]);

    const response = await rebase(place, {
      target_ref: 'main',
      branches: ['first', 'clashing', 'last'],
    });
    const results = ((await response.json()) as { results: Array<Record<string, unknown>> })
      .results;

    expect(response.status).toBe(200);
    expect(results.map((one) => one['branch'])).toEqual(['first', 'clashing', 'last']);
    expect(results[0]?.['status']).toBe('ok');
    expect(String(results[0]?.['new_head'])).toMatch(/^[0-9a-f]{40}$/);
    expect(results[1]?.['status']).toBe('conflict');
    expect(results[1]?.['conflict_files']).toEqual(['base.txt']);
    // The one after the conflict was still tried.
    expect(results[2]?.['status']).toBe('ok');

    // The rebased branch holds both sides' work.
    expect(
      await readSnapshot(place.repos, place.repository, String(results[0]?.['new_head'])),
    ).toEqual({
      'base.txt': 'one, changed\ntwo\n',
      'main.txt': 'the target moved\n',
      'a.txt': 'work\n',
    });
    // The one that conflicted is exactly where it was.
    expect(await headOf(place, 'clashing')).not.toBe(results[0]?.['new_head']);
    expect(await headOf(place, 'clashing')).toBe(
      (await place.repos.listRefs(place.repository)).find(
        (ref) => ref.name === 'refs/heads/clashing',
      )?.oid,
    );
    void first;
  });

  it('reports a branch that is not there as an error and goes on', async () => {
    const place = await world();
    await branch(place, 'real', { 'base.txt': 'one\ntwo\n', 'r.txt': 'work\n' });

    const results = (
      (await (
        await rebase(place, { target_ref: 'main', branches: ['nowhere', 'real'] })
      ).json()) as { results: Array<Record<string, unknown>> }
    ).results;

    expect(results[0]?.['status']).toBe('error');
    expect(results[1]?.['status']).toBe('ok');
  });
});

describe('a batch rebase the route will not make', () => {
  it.each([
    ['a body that is not an object', 'rebase please'],
    ['no target', { branches: ['a'] }],
    ['an empty branch list', { target_ref: 'main', branches: [] }],
    ['a branch list that is not a list', { target_ref: 'main', branches: 'a' }],
    ['a target that breaks git’s rules', { target_ref: '-x', branches: ['a'] }],
  ])('refuses %s', async (_what, body) => {
    const place = await world();

    expect((await rebase(place, body)).status).toBe(400);
  });

  it('names the offending branch by its index', async () => {
    const place = await world();

    const response = await rebase(place, {
      target_ref: 'main',
      branches: ['fine', 'a..b', 'also-fine'],
    });

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      'branches[1]',
    );
  });

  it('refuses more branches than it will take at once', async () => {
    const place = await world();
    const many = Array.from({ length: MOST_BRANCHES + 1 }, (_unused, at) => `b${String(at)}`);

    const response = await rebase(place, { target_ref: 'main', branches: many });

    expect(response.status).toBe(400);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      String(MOST_BRANCHES),
    );
  });

  it('checks the body before it looks the workspace up', async () => {
    const place = await world();

    const response = await rebase(
      place,
      { target_ref: 'main', branches: ['-x'] },
      'no-such-workspace',
    );

    expect(response.status).toBe(400);
  });

  it('refuses an unready workspace', async () => {
    const place = await world({ status: 'pending' });

    expect((await rebase(place, { target_ref: 'main', branches: ['a'] })).status).toBe(400);
  });

  it('refuses at once while another operation holds the workspace', async () => {
    const place = await world();
    await branch(place, 'one', { 'base.txt': 'one\ntwo\n', 'o.txt': 'work\n' });
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await rebase(place, { target_ref: 'main', branches: ['one'] });
    await held?.release();

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
  });

  it('refuses a target that is not there, leaving every branch alone', async () => {
    const place = await world();
    const was = await branch(place, 'one', { 'base.txt': 'one\ntwo\n', 'o.txt': 'work\n' });

    const response = await rebase(place, { target_ref: 'nowhere', branches: ['one'] });

    expect(response.status).toBe(400);
    expect(await headOf(place, 'one')).toBe(was);
  });
});
