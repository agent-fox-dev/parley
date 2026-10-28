import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';

import { repositoryOf } from '../repo/repository.js';
import { addMember } from '../store/identity.js';
import { type Service, identityService } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
  seedPatch,
} from '../testutil/identity.js';

/**
 * The branch push's checks, exercised through the route. Each refusal
 * on its own proves nothing about the order, so most of these ask for
 * two things to be wrong at once and say which answer belongs to the
 * earlier check.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

const TOKEN = 'a-token-for-origin';

interface World {
  service: Service;
  remote: FakeRemote;
  repos: RepositoryStore;
  slug: string;
  repository: string;
  owner: string;
  member: string;
  scopeless: string;
}

async function world(over: Parameters<typeof seedWorkspace>[1] | object = {}): Promise<World> {
  const remote = fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-branchpush-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, fetch: remote.fetch });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });

  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    git_url: `${remote.url}/origin.git`,
    ...(over as object),
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid, force: true },
    { name: 'refs/heads/feature', oldOid: null, newOid: first.oid, force: true },
    { name: 'refs/heads/fix/42-nil-map', oldOid: null, newOid: first.oid, force: true },
    { name: 'refs/heads/a%25b', oldOid: null, newOid: first.oid, force: true },
  ]);
  await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    scopeless: (await seedPat(service.store, owner, ['workspaces:read'])).presented,
  };
}

async function push(
  place: World,
  options: { segment?: string; slug?: string; credential?: string; body?: unknown } = {},
): Promise<Response> {
  const segment = options.segment ?? 'feature';
  return place.service.app.request(
    `/api/v1/workspaces/${options.slug ?? place.slug}/branches/${segment}/push`,
    {
      method: 'POST',
      headers: {
        authorization: `Bearer ${options.credential ?? place.owner}`,
        ...(options.body === undefined ? {} : { 'content-type': 'application/json' }),
      },
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
    },
  );
}

async function errorOf(
  response: Response,
): Promise<{ code: number; error_type?: string; message: string }> {
  const body = (await response.json()) as {
    error: { code: number; error_type?: string; message: string };
  };
  return body.error;
}

describe('the order the branch push checks run in', () => {
  it('answers a PAT without the scope before it looks the workspace up', async () => {
    const place = await world();

    const response = await push(place, {
      credential: place.scopeless,
      slug: 'no-such-workspace',
    });

    expect(response.status).toBe(403);
  });

  it('answers a member the access refusal, not the readiness one', async () => {
    const place = await world();
    await place.service.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
      'pending',
      place.slug,
    ]);

    const response = await push(place, { credential: place.member });

    expect(response.status).toBe(403);
  });

  it('answers an invalid name before an unready workspace', async () => {
    const place = await world();
    await place.service.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
      'pending',
      place.slug,
    ]);

    const response = await push(place, { segment: '-x' });

    expect(response.status).toBe(404);
    expect((await errorOf(response)).error_type).toBe('branch_not_found');
  });

  it('answers the patch-branch refusal although the workspace is busy', async () => {
    const place = await world({ workspace_mode: 'carry_patch', integration_branch: 'integration' });
    await seedPatch(place.service.store, place.slug, 'feature');
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await push(place);

    expect(response.status).toBe(400);
    expect((await errorOf(response)).error_type).toBe('patch_branch');
    await held?.release();
  });

  it('answers the missing branch only once the lock is taken', async () => {
    const place = await world();
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const busy = await push(place, { segment: 'nowhere' });
    expect((await errorOf(busy)).error_type).toBe('workspace_busy');

    await held?.release();
    const missing = await push(place, { segment: 'nowhere' });
    expect((await errorOf(missing)).error_type).toBe('branch_not_found');
  });
});

describe('the branch name in the path', () => {
  it.each([
    ['an ordinary name', 'feature', 'feature'],
    ['one with a slash escaped', 'fix%2F42-nil-map', 'fix/42-nil-map'],
    ['one with a percent escaped twice', 'a%2525b', 'a%25b'],
  ])('is one segment decoded exactly once: %s', async (_what, segment, expected) => {
    const place = await world();

    const response = await push(place, { segment });

    expect(response.status).toBe(200);
    expect(((await response.json()) as { branch: string }).branch).toBe(expected);
  });

  it('matches no route when the slash is not escaped', async () => {
    const place = await world();

    const response = await push(place, { segment: 'fix/42-nil-map' });

    expect(response.status).toBe(404);
  });
});

describe('what a branch push may ask for', () => {
  it.each([
    ['no body at all', undefined],
    ['an empty object', {}],
    ['fields nobody reads', { nothing: 'to see' }],
  ])('pushes without force for %s', async (_what, body) => {
    const place = await world();

    const response = await push(place, body === undefined ? {} : { body });

    expect(response.status).toBe(200);
    expect(((await response.json()) as { result: string }).result).toBe('pushed');
  });

  it.each([
    ['a body that is not an object', 'force'],
    ['a force that is not a boolean', { force: 'yes' }],
  ])('refuses %s', async (_what, body) => {
    const place = await world();

    expect((await push(place, { body })).status).toBe(400);
  });

  it.each(['pending', 'cloning', 'failed', 'archived'])(
    'refuses a workspace that is %s',
    async (status) => {
      const place = await world();
      await place.service.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', [
        status,
        place.slug,
      ]);

      expect((await push(place)).status).toBe(400);
    },
  );
});

describe('the branches the route will not send', () => {
  it.each([true, false])(
    'refuses the integration branch whether the lock is held (%s) or not',
    async (lockHeld) => {
      const place = await world({
        workspace_mode: 'carry_patch',
        integration_branch: 'feature',
      });
      const held = lockHeld ? await place.service.locks.take(`workspace:${place.slug}`) : undefined;

      const response = await push(place);

      expect((await errorOf(response)).error_type).toBe('integration_branch');
      await held?.release();
    },
  );

  it.each([true, false])(
    'refuses a registered patch branch whether the lock is held (%s) or not',
    async (lockHeld) => {
      const place = await world({
        workspace_mode: 'carry_patch',
        integration_branch: 'integration',
      });
      await seedPatch(place.service.store, place.slug, 'feature');
      const held = lockHeld ? await place.service.locks.take(`workspace:${place.slug}`) : undefined;

      expect((await errorOf(await push(place))).error_type).toBe('patch_branch');
      await held?.release();
    },
  );

  it("refuses the workspace's own branch with force", async () => {
    const place = await world({ branch: 'feature' });

    const response = await push(place, { body: { force: true } });

    expect((await errorOf(response)).error_type).toBe('protected_branch');
    expect(place.remote.pushed()).toEqual([]);
  });

  it('refuses a forced push of the branch HEAD follows where none is named', async () => {
    const place = await world({ branch: null });

    const response = await push(place, { segment: 'main', body: { force: true } });

    expect((await errorOf(response)).error_type).toBe('protected_branch');
    expect(place.remote.pushed()).toEqual([]);
  });

  it.each([true, false])('refuses a branch the repository lacks, force %s', async (force) => {
    const place = await world();

    const response = await push(place, { segment: 'nowhere', body: { force } });

    expect(response.status).toBe(404);
    expect((await errorOf(response)).error_type).toBe('branch_not_found');
    expect(place.remote.pushed()).toEqual([]);
  });
});

describe('a branch push against a busy workspace', () => {
  it('is refused at once, and succeeds once the lock is free', async () => {
    const place = await world();
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const refused = await push(place);
    expect(refused.status).toBe(409);
    expect((await errorOf(refused)).error_type).toBe('workspace_busy');

    await held?.release();
    expect((await push(place)).status).toBe(200);
  });
});
