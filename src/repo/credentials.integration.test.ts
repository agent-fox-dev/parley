import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { orgScope, userScope } from '../entries/store.js';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { resolveGitCredential } from './credentials.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

describe('where the reserved credential names are read from', () => {
  it("is the workspace's own scope, and no other", async () => {
    service = await identityService();
    const { store, secrets, app } = service;
    const owner = await seedUser(store);
    const org = await seedOrg(store, owner);
    await seedWorkspace(store, { slug: 'mine', owner_id: owner.id, org_id: org.id });
    const key = (await seedApiKey(store, owner)).presented;

    // The same reserved name, set everywhere but the workspace.
    await app.request(
      `${MOUNT}/user/secrets`,
      withJson('POST', { entries: [{ key: 'GIT_PAT', value: 'the-users-token' }] }, key),
    );
    await app.request(
      `${MOUNT}/orgs/${org.slug}/secrets`,
      withJson('POST', { entries: [{ key: 'GIT_PAT', value: 'the-orgs-token' }] }, key),
    );

    expect(await secrets.get(userScope(owner.id), 'GIT_PAT')).toBe('the-users-token');
    expect(await secrets.get(orgScope(org.id), 'GIT_PAT')).toBe('the-orgs-token');

    expect(await resolveGitCredential(secrets, workspaceScope('mine'), 'origin')).toBeUndefined();

    await app.request(
      `${MOUNT}/workspaces/mine/secrets`,
      withJson('POST', { entries: [{ key: 'GIT_PAT', value: 'the-workspaces-token' }] }, key),
    );

    expect(await resolveGitCredential(secrets, workspaceScope('mine'), 'origin')).toEqual({
      kind: 'token',
      token: 'the-workspaces-token',
    });
  });

  it('reads what the workspace create stored, through the same store', async () => {
    service = await identityService();
    const { store, secrets, app } = service;
    const owner = await seedUser(store);
    await seedOrg(store, owner);
    const key = (await seedApiKey(store, owner)).presented;

    await app.request(
      `${MOUNT}/workspaces`,
      withJson(
        'POST',
        {
          slug: 'with-a-pair',
          git_url: 'https://github.com/u/r.git',
          git_username: 'a-user',
          git_password: 'a-password',
        },
        key,
      ),
    );

    expect(await resolveGitCredential(secrets, workspaceScope('with-a-pair'), 'origin')).toEqual({
      kind: 'basic',
      username: 'a-user',
      password: 'a-password',
    });
  });
});
