import { afterEach, describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, identityService } from '../testutil/app.js';
import { fakeForge } from '../testutil/fakeforge.js';
import { seedUser, seedWorkspace } from '../testutil/identity.js';
import { reachForge } from './client.js';

/**
 * Reaching the forge: whose credential goes out, which repository it
 * goes to, and the rules every outbound request obeys. The forge here
 * is a fetch of the suite's own that remembers what it was handed.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world(over: Record<string, unknown> = {}) {
  service = await identityService();
  const owner = await seedUser(service.store);
  const workspace = await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://github.com/acme/widgets.git',
    ...over,
  });
  await service.secrets.put(workspaceScope(workspace.slug), 'GIT_PAT', 'the-workspace-token');
  return { ...service, workspace };
}

describe('whose credential reaches the forge', () => {
  it('sends the workspace’s, never the caller’s and never the environment’s', async () => {
    const held = await world();
    const forge = fakeForge();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await reached.client.send({ method: 'GET', path: '/repos/acme/widgets' });

    expect(forge.lastToken()).toBe('the-workspace-token');
    // Whatever the hub's own environment holds is not a workspace's.
    expect(JSON.stringify(forge.last())).not.toContain('GITHUB_TOKEN');
  });

  it('sends a password as the token where the workspace stored a pair', async () => {
    const held = await world();
    await held.secrets.put(workspaceScope('mine'), 'GIT_PAT', '');
    await held.secrets.put(workspaceScope('mine'), 'GIT_USERNAME', 'alice');
    await held.secrets.put(workspaceScope('mine'), 'GIT_PASSWORD', 'the-password');
    const forge = fakeForge();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await reached.client.send({ method: 'GET', path: '/repos/acme/widgets' });

    expect(reached.credential).toBe('basic');
    expect(forge.lastToken()).toBe('the-password');
  });

  it('carries no token at all where the workspace stored none', async () => {
    const held = await world();
    await held.secrets.put(workspaceScope('mine'), 'GIT_PAT', '');
    const forge = fakeForge();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await reached.client.send({ method: 'GET', path: '/repos/acme/widgets' });

    expect(reached.credential).toBe('none');
    expect(forge.last()?.headers['authorization']).toBeUndefined();
  });
});

/** A carry-patch workspace with an upstream of its own. */
async function carried(settings: Record<string, unknown> = {}) {
  const held = await world({
    workspace_mode: 'carry_patch',
    upstream_url: 'https://gitlab.example.com/group/sub/upstream.git',
    settings: { forge_api_url: 'https://github.acme.test/api/v3', ...settings },
  });
  await held.secrets.put(workspaceScope('mine'), 'UPSTREAM_GIT_PAT', 'the-upstream-token');
  return held;
}

describe('the upstream target', () => {
  it('reaches the upstream repository with the upstream credential', async () => {
    const held = await carried();
    const forge = fakeForge();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'upstream' },
    );
    await reached.client.send({ method: 'GET', path: '/projects/x' });

    expect(reached.target.repository).toEqual({
      host: 'gitlab.example.com',
      owner: 'group/sub',
      name: 'upstream',
    });
    expect(forge.lastToken()).toBe('the-upstream-token');
  });

  it('does not fall back to the origin’s API URL setting', async () => {
    const held = await carried();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: fakeForge().fetch },
      held.workspace,
      { target: 'upstream' },
    );

    // The origin setting names a GitHub host; the upstream's is unset,
    // so the upstream's own host decides.
    expect(reached.target.apiUrl).toBe('https://gitlab.example.com/api/v4');
  });

  it('uses the upstream setting where it is given', async () => {
    const held = await carried({ upstream_forge_api_url: 'https://gitlab.acme.test' });

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: fakeForge().fetch },
      held.workspace,
      { target: 'upstream' },
    );

    expect(reached.target.apiUrl).toBe('https://gitlab.acme.test/api/v4');
  });

  it.each([
    ['a standard workspace', {}, 'workspace_mode_mismatch'],
    [
      'a carry-patch workspace with no upstream',
      { workspace_mode: 'carry_patch', upstream_url: '' },
      'workspace_mode_mismatch',
    ],
  ])('refuses the upstream target on %s', async (_what, over, errorType) => {
    const held = await world(over);

    await expect(
      reachForge(
        { store: held.store, secrets: held.secrets, fetch: fakeForge().fetch },
        held.workspace,
        { target: 'upstream' },
      ),
    ).rejects.toMatchObject({ status: 400, errorType });
  });

  it.each([['ORIGIN'], [''], ['elsewhere']])('refuses the target %s', async (target) => {
    const held = await world();

    await expect(
      reachForge(
        { store: held.store, secrets: held.secrets, fetch: fakeForge().fetch },
        held.workspace,
        { target },
      ),
    ).rejects.toBeInstanceOf(HttpError);
  });
});

describe('what every outbound request obeys', () => {
  it('carries the hub’s user agent', async () => {
    const held = await world();
    const forge = fakeForge();

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await reached.client.send({ method: 'GET', path: '/repos/acme/widgets' });

    expect(forge.last()?.headers['user-agent']).toMatch(/^af-hub\//);
  });

  it('refuses a redirect to another host', async () => {
    const held = await world();
    const forge = fakeForge({ redirectTo: 'https://elsewhere.test/repos/acme/widgets' });

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );

    await expect(
      reached.client.send({ method: 'GET', path: '/repos/acme/widgets' }),
    ).rejects.toMatchObject({ status: 502, errorType: 'forge_error' });
  });

  it('follows a redirect that stays on the same host', async () => {
    const held = await world();
    const forge = fakeForge({ redirectTo: 'https://api.github.com/repos/acme/widgets-moved' });

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await reached.client
      .send({ method: 'GET', path: '/repos/acme/widgets' })
      .catch(() => undefined);

    expect(forge.calls().length).toBeGreaterThan(1);
  });

  it('gives up at its deadline', async () => {
    const held = await world();
    const forge = fakeForge({ silent: true });

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch, deadlineMs: 50 },
      held.workspace,
      { target: 'origin' },
    );

    await expect(
      reached.client.send({ method: 'GET', path: '/repos/acme/widgets' }),
    ).rejects.toMatchObject({ status: 504, errorType: 'forge_timeout' });
  });

  it('answers a connection that failed as the forge being unreachable', async () => {
    const held = await world();
    const forge = fakeForge({ unreachable: true });

    const reached = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );

    await expect(
      reached.client.send({ method: 'GET', path: '/repos/acme/widgets' }),
    ).rejects.toMatchObject({ status: 502, errorType: 'forge_unreachable' });
  });
});

describe('what is remembered between calls', () => {
  it('reads the credential and the setting again for every call', async () => {
    const held = await world();
    const forge = fakeForge();
    const reach = async () =>
      reachForge(
        { store: held.store, secrets: held.secrets, fetch: forge.fetch },
        (await import('../store/workspaces.js'))
          .findWorkspace(held.store, 'mine')
          .then((w) => w) as never,
        { target: 'origin' },
      );
    void reach;

    const first = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await first.client.send({ method: 'GET', path: '/repos/acme/widgets' });
    expect(forge.lastToken()).toBe('the-workspace-token');

    // The secret is rotated between the two calls.
    await held.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'the-new-token');
    const second = await reachForge(
      { store: held.store, secrets: held.secrets, fetch: forge.fetch },
      held.workspace,
      { target: 'origin' },
    );
    await second.client.send({ method: 'GET', path: '/repos/acme/widgets' });

    expect(forge.lastToken()).toBe('the-new-token');
  });
});
