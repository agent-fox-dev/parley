import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { ADMIN_TOKEN, type Service, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { addMember } from '../store/identity.js';
import { repositoryOf } from '../repo/repository.js';
import { decodePktLines, textOf } from './pktline.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function basic(password: string, username = 'parley'): string {
  return `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`;
}

/** Everything a git request needs: a ready workspace in an organization. */
async function world(): Promise<{
  service: Service;
  repos: RepositoryStore;
  repository: string;
  org: string;
  slug: string;
  ownerKey: string;
  path: string;
}> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-gitroutes-'));
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
  const key = await seedApiKey(service.store, owner);

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
    org: org.slug,
    slug: workspace.slug,
    ownerKey: key.presented,
    path: `/git/${org.slug}/${workspace.slug}.git`,
  };
}

/** The pkt-line messages a body carries, so a refusal can be compared. */
async function pktLines(response: Response): Promise<string[]> {
  const body = new Uint8Array(await response.arrayBuffer());
  return decodePktLines(body).map((line) => textOf(line));
}

describe('the address a repository is served at', () => {
  it('answers one identical not-found for every path that names no repository', async () => {
    const { service, org, slug, ownerKey } = await world();

    const answers = [];
    for (const path of [
      `/git/${org}/${slug}`,
      `/git/another-org/${slug}.git`,
      `/git/${org}/no-such-workspace.git`,
    ]) {
      const response = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
        headers: { authorization: basic(ownerKey) },
      });
      answers.push({
        status: response.status,
        lines: await pktLines(response),
      });
    }

    for (const answer of answers) {
      expect(answer.status).toBe(404);
      expect(answer.lines).toEqual(['ERR repository not found\n']);
    }
    expect(answers[1]).toEqual(answers[0]);
    expect(answers[2]).toEqual(answers[0]);
  });

  it('serves the advertisement at the path built from the two slugs', async () => {
    const { service, path, ownerKey } = await world();

    const response = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
      headers: { authorization: basic(ownerKey) },
    });

    expect(response.status).toBe(200);
    expect(response.headers.get('content-type')).toBe(
      'application/x-git-upload-pack-advertisement',
    );
    const lines = await pktLines(response);
    expect(lines[0]).toBe('# service=git-upload-pack\n');
  });
});

describe('the credential a git client offers', () => {
  it.each(['parley', '', 'anything-at-all', 'a@b.test'])(
    'is read from the Basic password whatever the username (%s)',
    async (username) => {
      const { service, path, ownerKey } = await world();

      const response = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
        headers: { authorization: basic(ownerKey, username) },
      });

      expect(response.status).toBe(200);
    },
  );

  it('is accepted as an API key, a PAT with the git scope and an admin token', async () => {
    const world0 = await world();
    const { service, path } = world0;
    const owner = await seedUser(service.store);
    // The three kinds, each on a workspace its access allows.
    const org = await seedOrg(service.store, owner);
    const workspace = await seedWorkspace(service.store, {
      owner_id: owner.id,
      org_id: org.id,
      status: 'ready',
    });
    await world0.repos.create(repositoryOf(workspace));
    const own = `/git/${org.slug}/${workspace.slug}.git/info/refs?service=git-upload-pack`;
    const key = await seedApiKey(service.store, owner);
    const pat = await seedPat(service.store, owner, ['git:read']);

    for (const credential of [key.presented, pat.presented]) {
      const response = await service.app.request(own, {
        headers: { authorization: basic(credential) },
      });
      expect(response.status, credential.slice(0, 7)).toBe(200);
    }

    const asAdmin = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
      headers: { authorization: basic(ADMIN_TOKEN) },
    });
    expect(asAdmin.status).toBe(200);
  });

  it.each([
    ['nothing at all', undefined],
    ['a credential nobody issued', 'af_key_0000000000000000.not-a-real-secret'],
    ['a store token', 'artifacts-token-the-hub-minted-for-itself'],
  ])('is refused identically when it is %s', async (_what, credential) => {
    const { service, path } = await world();

    const headers: Record<string, string> =
      credential === undefined ? {} : { authorization: basic(credential) };
    const response = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
      headers,
    });

    expect(response.status).toBe(401);
    // Without the challenge a git client never offers its credential.
    expect(response.headers.get('www-authenticate')).toBe('Basic realm="Parley"');
    expect(await pktLines(response)).toEqual(['ERR authentication required\n']);
  });

  it('is refused when the credential is revoked or its user is blocked', async () => {
    const { service, path } = await world();
    const blocked = await seedUser(service.store, { status: 'blocked' });
    const theirs = await seedApiKey(service.store, blocked);
    const owner = await seedUser(service.store);
    const revoked = await seedApiKey(service.store, owner, {
      revoked_at: '2026-03-01T00:00:00Z',
    });

    for (const credential of [theirs.presented, revoked.presented]) {
      const response = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
        headers: { authorization: basic(credential) },
      });
      expect(response.status).toBe(401);
      expect(await pktLines(response)).toEqual(['ERR authentication required\n']);
    }
  });
});

describe('what a git response carries', () => {
  it('never discloses a store token, on a served request or a refused one', async () => {
    const { service, path, ownerKey } = await world();
    const member = await seedUser(service.store);
    const [org] = path.split('/').slice(2);
    const orgs = await service.store.all<{ id: string }>('SELECT id FROM orgs WHERE slug = ?', [
      org ?? '',
    ]);
    await addMember(service.store, {
      org_id: orgs[0]?.id ?? '',
      user_id: member.id,
      role: 'member',
      created_at: SEED_NOW.toISOString(),
    });
    const theirs = await seedApiKey(service.store, member);

    const served = await service.app.request(`${path}/info/refs?service=git-upload-pack`, {
      headers: { authorization: basic(ownerKey) },
    });
    const refused = await service.app.request(`${path}/info/refs?service=git-receive-pack`, {
      headers: { authorization: basic(theirs.presented) },
    });

    for (const response of [served, refused]) {
      const text = new TextDecoder().decode(new Uint8Array(await response.arrayBuffer()));
      expect(text).not.toMatch(/artifacts|bearer|token/i);
      expect([...response.headers.keys()].join(' ')).not.toMatch(/token|authorization/i);
    }
  });
});

describe('the body a client sends', () => {
  it('is read when the client compressed it, as git does when it is worth it', async () => {
    const { service, path, ownerKey, repos, repository } = await world();
    const head = (await repos.listRefs(repository))[0]?.oid ?? '';
    const asked = new TextEncoder().encode(
      `${(`want ${head}\n`.length + 4).toString(16).padStart(4, '0')}want ${head}\n0000`,
    );
    const compressed = await new Response(
      new Response(asked as BodyInit).body?.pipeThrough(new CompressionStream('gzip')),
    ).arrayBuffer();

    const response = await service.app.request(`${path}/git-upload-pack`, {
      method: 'POST',
      body: compressed,
      headers: {
        authorization: basic(ownerKey),
        'content-type': 'application/x-git-upload-pack-request',
        'content-encoding': 'gzip',
      },
    });

    expect(response.status).toBe(200);
    const body = new Uint8Array(await response.arrayBuffer());
    expect(new TextDecoder().decode(body.subarray(0, 8))).toBe('0008NAK\n');
    expect(new TextDecoder().decode(body.subarray(8, 12))).toBe('PACK');
  });
});
