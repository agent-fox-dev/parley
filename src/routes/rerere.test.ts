import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { conflictIdentity } from '../history/conflict.js';
import { resolutionStore } from '../history/resolutions.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { seedApiKey, seedClock, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { readPathspec } from './rerere.js';

/**
 * The resolutions the hub remembers, and forgetting one. What makes
 * the forget worth testing is what it causes: the next occurrence of
 * that conflict is reported rather than settled.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  slug: string;
  repository: string;
  credential: string;
}

async function world(over: object = {}): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-rerere-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const service = await identityService([], { repos: localRepositoryStore(dir) });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    ...over,
  });

  return {
    service,
    slug: workspace.slug,
    repository: repositoryOf(workspace),
    credential: (await seedApiKey(service.store, owner)).presented,
  };
}

const conflict = (path: string, line: string) => ({
  path,
  base: 'one\ntwo\n',
  ours: `${line}\ntwo\n`,
  theirs: 'other\ntwo\n',
});

async function record(place: World, path: string, line: string): Promise<string> {
  return resolutionStore(place.service.store, seedClock()).record(
    place.repository,
    conflict(path, line),
    'settled\ntwo\n',
  );
}

async function list(place: World): Promise<Array<Record<string, unknown>>> {
  const response = await place.service.app.request(`/api/v1/workspaces/${place.slug}/rerere`, {
    headers: { authorization: `Bearer ${place.credential}` },
  });
  return ((await response.json()) as { resolutions: Array<Record<string, unknown>> }).resolutions;
}

async function forget(place: World, pathspec: string): Promise<Response> {
  return place.service.app.request(
    `/api/v1/workspaces/${place.slug}/rerere/${encodeURIComponent(pathspec).replaceAll('%2F', '/')}`,
    { method: 'DELETE', headers: { authorization: `Bearer ${place.credential}` } },
  );
}

describe('the resolutions a workspace has recorded', () => {
  it('are listed with their identity, path and whether they will be replayed', async () => {
    const place = await world();
    const identity = await record(place, 'src/config.go', 'ours');

    const listed = await list(place);

    expect(listed).toHaveLength(1);
    expect(listed[0]?.['id']).toBe(identity);
    expect(listed[0]?.['path']).toBe('src/config.go');
    expect(listed[0]?.['resolved']).toBe(true);
    expect(String(listed[0]?.['recorded_at'])).not.toHaveLength(0);
  });

  it('are an empty list where there are none', async () => {
    expect(await list(await world())).toEqual([]);
  });
});

describe('forgetting a resolution', () => {
  it('removes the one an identity names', async () => {
    const place = await world();
    const identity = await record(place, 'src/config.go', 'ours');
    await record(place, 'src/other.go', 'theirs');

    const response = await forget(place, identity);

    expect(response.status).toBe(204);
    expect((await list(place)).map((one) => one['path'])).toEqual(['src/other.go']);
  });

  it('removes every one recorded at a path', async () => {
    const place = await world();
    await record(place, 'src/config.go', 'ours');
    await record(place, 'src/config.go', 'another');
    await record(place, 'src/other.go', 'theirs');

    const response = await forget(place, 'src/config.go');

    expect(response.status).toBe(204);
    expect((await list(place)).map((one) => one['path'])).toEqual(['src/other.go']);
  });

  it('means the conflict is reported again rather than replayed', async () => {
    const place = await world();
    const one = conflict('src/config.go', 'ours');
    const resolutions = resolutionStore(place.service.store, seedClock());
    await record(place, 'src/config.go', 'ours');

    expect(await resolutions.replay(place.repository, one)).toBe('settled\ntwo\n');

    await forget(place, await conflictIdentity(one));

    expect(await resolutions.replay(place.repository, one)).toBeUndefined();
  });
});

describe('a forget the route will not make', () => {
  it.each([
    ['an empty pathspec', '   '],
    ['one that is not a valid escape', '%zz'],
  ])('answers 400 for %s, before it looks the workspace up', (_what, pathspec) => {
    expect(() => readPathspec(pathspec)).toThrow(/pathspec/);
  });

  it('answers 400 for a malformed pathspec whatever workspace it names', async () => {
    const place = await world();

    const response = await place.service.app.request(
      `/api/v1/workspaces/no-such-workspace/rerere/%zz`,
      { method: 'DELETE', headers: { authorization: `Bearer ${place.credential}` } },
    );

    expect(response.status).toBe(400);
  });

  it.each([
    ['an identity nobody recorded', 'a'.repeat(64)],
    ['a path nobody recorded', 'src/nowhere.go'],
  ])('answers 404 for %s', async (_what, pathspec) => {
    const place = await world();
    await record(place, 'src/config.go', 'ours');

    expect((await forget(place, pathspec)).status).toBe(404);
    expect(await list(place)).toHaveLength(1);
  });

  it('answers 409 workspace_busy while another operation holds the workspace', async () => {
    const place = await world();
    await record(place, 'src/config.go', 'ours');
    const held = await place.service.locks.take(`workspace:${place.slug}`);

    const response = await forget(place, 'src/config.go');
    await held?.release();

    expect(response.status).toBe(409);
    expect(((await response.json()) as { error: { error_type: string } }).error.error_type).toBe(
      'workspace_busy',
    );
    expect(await list(place)).toHaveLength(1);
  });

  it('keeps the slashes of a path while reading everything else escaped', () => {
    expect(readPathspec('src/a%20b.go')).toBe('src/a b.go');
    expect(readPathspec('src/deep/nested.go')).toBe('src/deep/nested.go');
  });
});
