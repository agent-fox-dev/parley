import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { repositoryOf } from '../repo/repository.js';
import { type Service, identityService } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The branch push against a remote that really serves the protocol.
 * What the route answers and what origin ends up holding are checked
 * together: a result of "pushed" for a branch that never arrived is
 * the failure this route exists to avoid.
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
  credential: string;
  tip: string;
}

async function world(over: object = {}): Promise<World> {
  const remote = fakeRemote({ requires: `Bearer ${TOKEN}` });
  cleanups.push(remote.dispose);
  await remote.ready;

  const dir = mkdtempSync(join(tmpdir(), 'parley-push-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  const service = await identityService([], { repos, fetch: remote.fetch });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
    git_url: `${remote.url}/origin.git`,
    ...over,
  });

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  const second = await buildCommit(
    repos,
    repository,
    { 'a.txt': 'hello\n', 'b.txt': 'more\n' },
    [first.oid],
    'second',
  );
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid, force: true },
    { name: 'refs/heads/feature', oldOid: null, newOid: second.oid, force: true },
  ]);
  await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', TOKEN);

  return {
    service,
    remote,
    repos,
    slug: workspace.slug,
    repository,
    credential: (await seedApiKey(service.store, owner)).presented,
    tip: second.oid,
  };
}

async function push(place: World, branch = 'feature', body?: unknown): Promise<Response> {
  return place.service.app.request(`/api/v1/workspaces/${place.slug}/branches/${branch}/push`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${place.credential}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function onOrigin(place: World, branch: string): Promise<string | undefined> {
  return (await place.remote.store.listRefs(place.remote.repository)).find(
    (ref) => ref.name === `refs/heads/${branch}`,
  )?.oid;
}

describe('a branch pushed to origin', () => {
  it('answers with the branch, the commit offered and where it went', async () => {
    const place = await world();

    const response = await push(place);
    const answered = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(answered['branch']).toBe('feature');
    expect(answered['sha']).toBe(place.tip);
    expect(String(answered['sha'])).toMatch(/^[0-9a-f]{40}$/);
    expect(answered['remote']).toBe('origin');
    expect(answered['remote_url']).toBe(`${place.remote.url}/origin.git`);
    expect(answered['result']).toBe('pushed');

    // And origin really holds it.
    expect(await onOrigin(place, 'feature')).toBe(place.tip);
  });

  it('reports up_to_date when origin already had the commit, with force and without', async () => {
    const place = await world();
    expect((await push(place)).status).toBe(200);

    const again = (await (await push(place)).json()) as { result: string };
    const forced = (await (await push(place, 'feature', { force: true })).json()) as {
      result: string;
    };

    expect(again.result).toBe('up_to_date');
    expect(forced.result).toBe('up_to_date');
  });

  it('reports forced when origin had a different commit and force was given', async () => {
    const place = await world();
    // Origin has the branch at a commit the hub's does not descend from.
    const theirs = await buildCommit(
      place.remote.store,
      place.remote.repository,
      { 'a.txt': 'origin wrote this\n' },
      [],
      'origin only',
    );
    await place.remote.store.updateRefs(place.remote.repository, [
      { name: 'refs/heads/feature', oldOid: null, newOid: theirs.oid, force: true },
    ]);

    const response = await push(place, 'feature', { force: true });

    expect(response.status).toBe(200);
    expect(((await response.json()) as { result: string }).result).toBe('forced');
    expect(await onOrigin(place, 'feature')).toBe(place.tip);
  });
});

describe('the address a push reports', () => {
  it.each([
    [
      'one carrying a credential',
      'https://someone:secret@example.test/r.git',
      'https://example.test/r.git',
    ],
    ['a plain one', 'https://example.test/r.git', 'https://example.test/r.git'],
    [
      'an scp-style address',
      'git@example.test:acme/widgets.git',
      'git@example.test:acme/widgets.git',
    ],
  ])('has no userinfo in it: %s', async (_what, gitUrl, expected) => {
    const { withoutUserinfo } = await import('./branch_push.js');

    expect(withoutUserinfo(gitUrl)).toBe(expected);
  });

  it('reports the stored address, scrubbed, in a real answer', async () => {
    const place = await world();
    await place.service.store.run('UPDATE workspaces SET git_url = ? WHERE slug = ?', [
      `${place.remote.url.replace('http://', 'http://someone:secret@')}/origin.git`,
      place.slug,
    ]);

    const response = await push(place);

    expect(response.status).toBe(200);
    const answered = (await response.json()) as { remote_url: string };
    expect(answered.remote_url).not.toContain('secret');
    expect(answered.remote_url).toContain('/origin.git');
  });
});

describe('a push whose caller goes away', () => {
  it('finishes, and the next call sees it done', async () => {
    const place = await world();
    const controller = new AbortController();

    // The caller sends the request and leaves before the answer.
    const sent = Promise.resolve(
      place.service.app.request(`/api/v1/workspaces/${place.slug}/branches/feature/push`, {
        method: 'POST',
        headers: { authorization: `Bearer ${place.credential}` },
        signal: controller.signal,
      }),
    );
    controller.abort();
    await sent.catch(() => undefined);

    // The branch still reached origin, and the route says so.
    for (
      let round = 0;
      round < 50 && (await onOrigin(place, 'feature')) === undefined;
      round += 1
    ) {
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    expect(await onOrigin(place, 'feature')).toBe(place.tip);
    expect(((await (await push(place)).json()) as { result: string }).result).toBe('up_to_date');
  });
});

describe('what the platform is asked to keep running', () => {
  it('hands the push to the execution context, so a cancelled request still finishes', async () => {
    const place = await world();
    const kept: Array<Promise<unknown>> = [];

    const response = await place.service.app.request(
      `/api/v1/workspaces/${place.slug}/branches/feature/push`,
      { method: 'POST', headers: { authorization: `Bearer ${place.credential}` } },
      {},
      {
        waitUntil: (work: Promise<unknown>) => {
          kept.push(work);
        },
        passThroughOnException: () => undefined,
      } as unknown as ExecutionContext,
    );

    expect(response.status).toBe(200);
    expect(kept, 'the push was handed to the platform').toHaveLength(1);
    await Promise.all(kept);
    expect(await onOrigin(place, 'feature')).toBe(place.tip);
  });
});
