import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { addMember } from '../store/identity.js';
import { type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { decodePktLines, encodePkt, encodeSpecial, FLUSH, concatBytes, textOf } from './pktline.js';
import { ZERO_OID } from './transport.js';
import { reportOf, servePush } from './push.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function basic(password: string): string {
  return `Basic ${Buffer.from(`parley:${password}`).toString('base64')}`;
}

/** A push body: the ref commands a client sends, with no pack behind them. */
function commandList(commands: readonly RefChange[]): Uint8Array {
  return concatBytes([
    ...commands.map((command, index) =>
      encodePkt(
        `${command.oldOid ?? ZERO_OID} ${command.newOid ?? ZERO_OID} ${command.name}${
          index === 0 ? '\u0000report-status' : ''
        }\n`,
      ),
    ),
    encodeSpecial(FLUSH),
  ]);
}

interface World {
  service: Service;
  repos: RepositoryStore;
  repository: string;
  path: string;
  head: string;
  owner: string;
  member: string;
  stranger: string;
  scopeless: string;
}

async function world(): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-gitpush-'));
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

  return {
    service,
    repos,
    repository,
    path: `/git/${org.slug}/${workspace.slug}.git/git-receive-pack`,
    head: first.oid,
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    stranger: (await seedApiKey(service.store, stranger)).presented,
    scopeless: (await seedPat(service.store, owner, [])).presented,
  };
}

describe('the report a push receives', () => {
  it('says the pack was taken and names every ref, accepted or not', () => {
    const lines = decodePktLines(
      reportOf([
        { name: 'refs/heads/one', ok: true },
        { name: 'refs/heads/two', ok: false, reason: 'because this suite said so' },
      ]),
    ).map((line) => textOf(line));

    expect(lines).toEqual([
      'unpack ok\n',
      'ok refs/heads/one\n',
      'ng refs/heads/two because this suite said so\n',
      '',
    ]);
  });
});

describe('a push whose refs are decided one by one', () => {
  it('applies exactly the refs it reported ok', async () => {
    const place = await world();
    const second = await buildCommit(
      place.repos,
      place.repository,
      { 'a.txt': 'hello\n', 'b.txt': 'more\n' },
      [place.head],
      'second',
    );
    const before = await place.repos.listRefs(place.repository);

    const commands: RefChange[] = [
      { name: 'refs/heads/accepted', oldOid: null, newOid: second.oid },
      { name: 'refs/heads/refused', oldOid: null, newOid: second.oid },
      { name: 'refs/heads/also-accepted', oldOid: null, newOid: second.oid },
    ];

    const report = await servePush({
      repos: place.repos,
      repository: place.repository,
      commands,
      pack: new Uint8Array(),
      decide: (command) =>
        Promise.resolve(
          command.name === 'refs/heads/refused'
            ? { accept: false as const, reason: 'this suite refused it' }
            : { accept: true as const },
        ),
    });

    const lines = decodePktLines(report).map((line) => textOf(line));
    expect(lines).toContain('ok refs/heads/accepted\n');
    expect(lines).toContain('ok refs/heads/also-accepted\n');
    expect(lines).toContain('ng refs/heads/refused this suite refused it\n');

    const after = await place.repos.listRefs(place.repository);
    const changed = after
      .filter((ref) => !before.some((was) => was.name === ref.name && was.oid === ref.oid))
      .map((ref) => ref.name)
      .toSorted();
    expect(changed).toEqual(['refs/heads/accepted', 'refs/heads/also-accepted']);
  });

  it('reports every ref as refused when the store will not take the pack', async () => {
    const place = await world();
    const before = await place.repos.listRefs(place.repository);
    const refusing: RepositoryStore = {
      ...place.repos,
      receivePack: () => Promise.reject(new Error('the store would not take it')),
      updateRefs: () => Promise.reject(new Error('the store would not take it')),
    };

    const report = await servePush({
      repos: refusing,
      repository: place.repository,
      commands: [{ name: 'refs/heads/one', oldOid: null, newOid: place.head }],
      pack: new Uint8Array(),
    });

    const lines = decodePktLines(report).map((line) => textOf(line));
    expect(lines[0]).toBe('unpack ok\n');
    expect(lines[1]).toMatch(/^ng refs\/heads\/one /);
    expect(await place.repos.listRefs(place.repository)).toEqual(before);
  });
});

describe('a push the service refuses outright', () => {
  it.each([
    ['no credential at all', undefined, 401],
    ['a member', 'member', 403],
    ['a stranger', 'stranger', 404],
    ['a token without the git scope', 'scopeless', 403],
  ])('changes nothing and reaches no store: %s', async (_what, who, status) => {
    const place = await world();
    const before = await place.repos.listRefs(place.repository);
    let reached = 0;
    const watched: RepositoryStore = {
      ...place.repos,
      receivePack: async (...args) => {
        reached += 1;
        return place.repos.receivePack(...args);
      },
      updateRefs: async (...args) => {
        reached += 1;
        return place.repos.updateRefs(...args);
      },
    };
    const service = await identityService([], { repos: watched });
    cleanups.push(service.dispose);

    const credential = who === undefined ? undefined : place[who as 'member'];
    const headers: Record<string, string> =
      credential === undefined ? {} : { authorization: basic(credential) };
    const response = await place.service.app.request(place.path, {
      method: 'POST',
      body: commandList([{ name: 'refs/heads/new', oldOid: null, newOid: place.head }]),
      headers,
    });

    expect(response.status).toBe(status);
    expect(await place.repos.listRefs(place.repository)).toEqual(before);
    expect(reached).toBe(0);
  });
});
