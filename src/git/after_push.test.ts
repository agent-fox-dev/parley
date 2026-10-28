import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { workspaceScope } from '../platform/secrets.js';
import { addPatch } from '../patches/registry.js';
import { repositoryOf } from '../repo/repository.js';
import { findWorkspace } from '../store/workspaces.js';
import { identityService } from '../testutil/app.js';
import { fakeRemote } from '../testutil/fakeremote.js';
import { buildCommit } from '../testutil/history.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { FLUSH, concatBytes, encodePkt, encodeSpecial } from './pktline.js';
import { ZERO_OID } from './transport.js';
import { headRef } from './after_push.js';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

describe('which ref a workspace reports as its head', () => {
  it.each([
    ['main, where there is one', ['refs/heads/other', 'refs/heads/main'], 'refs/heads/main'],
    ['master, failing that', ['refs/heads/other', 'refs/heads/master'], 'refs/heads/master'],
    ['the first branch, failing both', ['refs/heads/only'], 'refs/heads/only'],
  ])('is %s', (_what, names, expected) => {
    const refs = names.map((name, index) => ({ name, oid: String(index).repeat(40) }));

    expect(headRef(refs)?.name).toBe(expected);
  });

  it('is nothing at all where the repository has no branches', () => {
    expect(headRef([{ name: 'refs/tags/v1', oid: 'a'.repeat(40) }])).toBeUndefined();
  });
});

describe('a push whose every ref was refused', () => {
  it('reaches no fork and changes nothing but the head', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'parley-afterpush-'));
    cleanups.push(() => {
      rmSync(dir, { recursive: true, force: true });
    });
    const remote = fakeRemote();
    cleanups.push(remote.dispose);
    const repos = localRepositoryStore(dir);
    const service = await identityService([], { repos, fetch: remote.fetch });
    cleanups.push(service.dispose);

    const owner = await seedUser(service.store);
    const org = await seedOrg(service.store, owner);
    const workspace = await seedWorkspace(service.store, {
      owner_id: owner.id,
      org_id: org.id,
      status: 'ready',
      workspace_mode: 'carry_patch',
      git_url: `${remote.url}/fork.git`,
      settings: { patch_branch_source: 'origin', push_patches_to_origin: false },
    });
    await addPatch(service.store, workspace.slug, 'a-patch', SEED_NOW.toISOString());
    await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', 'a-token');

    const repository = repositoryOf(workspace);
    await repos.create(repository);
    const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
    await repos.updateRefs(repository, [
      { name: 'refs/heads/main', oldOid: null, newOid: first.oid },
    ]);
    const credential = await seedApiKey(service.store, owner);
    const before = await findWorkspace(service.store, workspace.slug);

    const response = await service.app.request(
      `/git/${org.slug}/${workspace.slug}.git/git-receive-pack`,
      {
        method: 'POST',
        body: concatBytes([
          encodePkt(`${ZERO_OID} ${first.oid} refs/heads/a-patch\u0000report-status\n`),
          encodeSpecial(FLUSH),
        ]),
        headers: {
          authorization: `Basic ${Buffer.from(`parley:${credential.presented}`).toString('base64')}`,
        },
      },
    );

    expect(response.status).toBe(200);
    expect(remote.requests()).toBe(0);

    const after = await findWorkspace(service.store, workspace.slug);
    // The head is now known; nothing else about the workspace moved.
    expect(after?.head_sha).toBe(first.oid);
    const differing = Object.keys(after ?? {}).filter(
      (key) =>
        JSON.stringify((after as unknown as Record<string, unknown>)[key]) !==
        JSON.stringify((before as unknown as Record<string, unknown>)[key]),
    );
    expect(differing).toEqual(['head_sha']);

    // And the refused ref is not there.
    const refs = await repos.listRefs(repository);
    expect(refs.some((ref) => ref.name === 'refs/heads/a-patch')).toBe(false);
  });
});
