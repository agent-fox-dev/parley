import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { RelationalStore } from '../platform/index.js';
import { repositoryOf } from '../repo/repository.js';
import { addMember } from '../store/identity.js';
import { ADMIN_TOKEN, type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { decodePktLines, textOf } from './pktline.js';

const NOT_FOUND = 'ERR repository not found\n';
const MEMBERS = 'ERR org members have read-only access to this workspace\n';
const NOT_AVAILABLE = 'ERR repository not available\n';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function basic(password: string): string {
  return `Basic ${Buffer.from(`parley:${password}`).toString('base64')}`;
}

async function pktLines(response: Response): Promise<string[]> {
  return decodePktLines(new Uint8Array(await response.arrayBuffer())).map((line) => textOf(line));
}

interface World {
  service: Service;
  store: RelationalStore;
  repos: RepositoryStore;
  org: string;
  slug: string;
  repository: string;
  head: string;
  owner: string;
  member: string;
  stranger: string;
  memberWrite: string;
  bound: string;
}

/** A ready workspace, its owner, a member, a stranger and their tokens. */
async function world(): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-gitaccess-'));
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
  });
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(service.store);

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid },
  ]);

  const elsewhere = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  const boundToken = await seedBoundToken(service.store, owner, elsewhere.slug, [
    'git:read',
    'git:write',
  ]);

  return {
    service,
    store: service.store,
    repos,
    org: org.slug,
    slug: workspace.slug,
    repository,
    head: first.oid,
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    stranger: (await seedApiKey(service.store, stranger)).presented,
    memberWrite: (await seedPat(service.store, member, ['git:read', 'git:write'])).presented,
    bound: boundToken.presented,
  };
}

/** The ref advertisement for one of the two services. */
async function advertise(place: World, credential: string, service: string): Promise<Response> {
  return place.service.app.request(
    `/git/${place.org}/${place.slug}.git/info/refs?service=${service}`,
    { headers: { authorization: basic(credential) } },
  );
}

describe('who may fetch and who may push', () => {
  it('refuses a member the receive advertisement and the push, whatever their scopes', async () => {
    const place = await world();

    for (const credential of [place.member, place.memberWrite]) {
      const advertisement = await advertise(place, credential, 'git-receive-pack');
      expect(advertisement.status).toBe(403);
      expect(await pktLines(advertisement)).toEqual([MEMBERS]);

      const pushed = await place.service.app.request(
        `/git/${place.org}/${place.slug}.git/git-receive-pack`,
        { method: 'POST', body: new Uint8Array(), headers: { authorization: basic(credential) } },
      );
      expect(pushed.status).toBe(403);
      expect(await pktLines(pushed)).toEqual([MEMBERS]);
    }

    // And the repository is where it was.
    expect(await place.repos.listRefs(place.repository)).toContainEqual({
      name: 'refs/heads/main',
      oid: place.head,
    });
  });

  it('serves a member the fetch advertisement, as it serves the owner', async () => {
    const place = await world();

    const asOwner = await advertise(place, place.owner, 'git-upload-pack');
    const asMember = await advertise(place, place.member, 'git-upload-pack');

    expect(asMember.status).toBe(200);
    expect(await pktLines(asMember)).toEqual(await pktLines(asOwner));
  });

  it('serves an admin token the fetch and the push', async () => {
    const place = await world();

    expect((await advertise(place, ADMIN_TOKEN, 'git-upload-pack')).status).toBe(200);
    expect((await advertise(place, ADMIN_TOKEN, 'git-receive-pack')).status).toBe(200);
  });

  it('tells a stranger and a token bound elsewhere that there is no repository', async () => {
    const place = await world();
    const archived = await seedWorkspace(place.store, {
      owner_id: 'nobody',
      status: 'archived',
    });

    const cases = [
      ['a stranger, the workspace', place.stranger, place.slug],
      ['a stranger, a slug naming nothing', place.stranger, 'no-such-workspace'],
      ['a stranger, an archived workspace', place.stranger, archived.slug],
      ['a token bound elsewhere', place.bound, place.slug],
    ] as const;

    const answers: Array<{ status: number; lines: string[] }> = [];
    for (const [, credential, slug] of cases) {
      for (const service of ['git-upload-pack', 'git-receive-pack']) {
        const response = await place.service.app.request(
          `/git/${place.org}/${slug}.git/info/refs?service=${service}`,
          { headers: { authorization: basic(credential) } },
        );
        answers.push({ status: response.status, lines: await pktLines(response) });
      }
    }

    for (const answer of answers) {
      expect(answer).toEqual({ status: 404, lines: [NOT_FOUND] });
    }
  });
});

describe('the order the git checks run in', () => {
  it('answers a mismatched organization before it considers readiness', async () => {
    const place = await world();
    const unready = await seedWorkspace(place.store, {
      owner_id: 'nobody',
      status: 'pending',
    });

    const response = await place.service.app.request(
      `/git/${place.org}/${unready.slug}.git/info/refs?service=git-upload-pack`,
      { headers: { authorization: basic(place.owner) } },
    );

    expect(await pktLines(response)).toEqual([NOT_FOUND]);
  });

  it('answers an unready repository before it considers access', async () => {
    const place = await world();
    const orgs = await place.store.all<{ id: string }>('SELECT id FROM orgs WHERE slug = ?', [
      place.org,
    ]);
    const unready = await seedWorkspace(place.store, {
      owner_id: 'nobody',
      org_id: orgs[0]?.id ?? '',
      status: 'cloning',
    });

    // The caller is a member, who would be refused a push on access
    // alone; the readiness answer comes first.
    const response = await place.service.app.request(
      `/git/${place.org}/${unready.slug}.git/info/refs?service=git-receive-pack`,
      { headers: { authorization: basic(place.member) } },
    );

    expect(await pktLines(response)).toEqual([NOT_AVAILABLE]);
  });

  it("refuses a member's push as a member before it looks at the scopes", async () => {
    const place = await world();
    const member = await seedUser(place.store);
    const orgs = await place.store.all<{ id: string }>('SELECT id FROM orgs WHERE slug = ?', [
      place.org,
    ]);
    await addMember(place.store, {
      org_id: orgs[0]?.id ?? '',
      user_id: member.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    const scopeless = await seedPat(place.store, member, []);

    const response = await advertise(place, scopeless.presented, 'git-receive-pack');

    expect(await pktLines(response)).toEqual([MEMBERS]);
  });
});

describe('the git scopes a personal access token holds', () => {
  it.each([
    ['git:read alone', ['git:read'], 200, 403],
    ['git:write alone', ['git:write'], 200, 200],
    ['neither', [], 403, 403],
  ])('decides what it may do: %s', async (_what, scopes, fetching, pushing) => {
    const place = await world();
    const owners = await place.store.all<{ owner_id: string }>(
      'SELECT owner_id FROM workspaces WHERE slug = ?',
      [place.slug],
    );
    const owner = { id: owners[0]?.owner_id ?? '' };
    const pat = await seedPat(place.store, owner as never, scopes);

    expect((await advertise(place, pat.presented, 'git-upload-pack')).status).toBe(fetching);
    expect((await advertise(place, pat.presented, 'git-receive-pack')).status).toBe(pushing);
  });
});

describe('a membership lookup that fails', () => {
  it('refuses every git request rather than granting a level', async () => {
    const place = await world();
    // The store answers everything but the membership question.
    const original = place.store.all.bind(place.store);
    place.store.all = (async (sql: string, params?: readonly unknown[]) => {
      if (sql.includes('org_members')) {
        throw new Error('the membership table is unavailable');
      }
      return original(sql, params as never);
    }) as typeof place.store.all;

    for (const credential of [place.member, place.stranger]) {
      for (const service of ['git-upload-pack', 'git-receive-pack']) {
        const response = await advertise(place, credential, service);
        expect(response.status, `${service}`).toBe(500);
        expect((await pktLines(response))[0]).toMatch(/^ERR /);
      }
    }

    place.store.all = original;
    expect(await place.repos.listRefs(place.repository)).toContainEqual({
      name: 'refs/heads/main',
      oid: place.head,
    });
  });
});
