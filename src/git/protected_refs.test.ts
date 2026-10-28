import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { ADMIN_TOKEN, identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedPat, seedUser, seedWorkspace } from '../testutil/identity.js';
import { FLUSH, concatBytes, decodePktLines, encodePkt, encodeSpecial, textOf } from './pktline.js';
import { ZERO_OID } from './transport.js';
import { PROTECTED_MESSAGE, isProtected, protectedRefs } from './protected_refs.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

describe('the refs the hub maintains', () => {
  it.each([
    ['a replaced tip', 'refs/hub/replaced/a-branch', true],
    ['a nested one', 'refs/hub/replaced/team/a-branch', true],
    ['the prefix itself as a ref', 'refs/hub/replaced/', true],
    ['an ordinary branch', 'refs/heads/main', false],
    ['another hub ref', 'refs/hub/engine/merge', false],
    ['a branch that merely looks alike', 'refs/heads/hub/replaced/x', false],
  ])('recognises %s', (_what, ref, expected) => {
    expect(isProtected(ref)).toBe(expected);
  });

  it('refuses a create, an update and a delete alike', async () => {
    const decide = protectedRefs();
    const ref = 'refs/hub/replaced/a-branch';
    const oid = 'a'.repeat(40);

    const outcomes = await Promise.all([
      decide({ name: ref, oldOid: null, newOid: oid }),
      decide({ name: ref, oldOid: 'b'.repeat(40), newOid: oid }),
      decide({ name: ref, oldOid: oid, newOid: null }),
    ]);

    for (const outcome of outcomes) {
      expect(outcome).toEqual({ accept: false, reason: PROTECTED_MESSAGE });
    }
  });

  it('says nothing about a ref outside the namespace', async () => {
    expect(
      await protectedRefs()({ name: 'refs/heads/main', oldOid: null, newOid: 'c'.repeat(40) }),
    ).toEqual({ accept: true });
  });
});

/** A push body carrying one ref command and no pack. */
function commandList(name: string, oldOid: string, newOid: string): Uint8Array {
  return concatBytes([
    encodePkt(`${oldOid} ${newOid} ${name}\u0000report-status\n`),
    encodeSpecial(FLUSH),
  ]);
}

describe('a push to the protected namespace', () => {
  it.each([
    ['a standard workspace, an API key', 'standard', 'key'],
    ['a standard workspace, a PAT with git:write', 'standard', 'pat'],
    ['a standard workspace, an admin token', 'standard', 'admin'],
    ['a carry-patch workspace, an API key', 'carry_patch', 'key'],
    ['a carry-patch workspace, a PAT with git:write', 'carry_patch', 'pat'],
    ['a carry-patch workspace, an admin token', 'carry_patch', 'admin'],
  ])('is refused the same way: %s', async (_what, mode, which) => {
    const dir = mkdtempSync(join(tmpdir(), 'parley-protected-'));
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
      workspace_mode: mode as 'standard',
    });
    const repository = repositoryOf(workspace);
    await repos.create(repository);
    const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
    const held = 'refs/hub/replaced/a-branch';
    await repos.updateRefs(repository, [{ name: held, oldOid: null, newOid: first.oid }]);
    const before = await repos.listRefs(repository);

    const credential =
      which === 'admin'
        ? ADMIN_TOKEN
        : which === 'pat'
          ? (await seedPat(service.store, owner, ['git:write'])).presented
          : (await seedApiKey(service.store, owner)).presented;

    const response = await service.app.request(
      `/git/${org.slug}/${workspace.slug}.git/git-receive-pack`,
      {
        method: 'POST',
        body: commandList(held, first.oid, ZERO_OID),
        headers: {
          authorization: `Basic ${Buffer.from(`parley:${credential}`).toString('base64')}`,
        },
      },
    );

    expect(response.status).toBe(200);
    const lines = decodePktLines(new Uint8Array(await response.arrayBuffer())).map((line) =>
      textOf(line),
    );
    expect(lines[0]).toBe('unpack ok\n');
    expect(lines[1]).toBe(`ng ${held} ${PROTECTED_MESSAGE}\n`);
    expect(await repos.listRefs(repository)).toEqual(before);
  });
});
