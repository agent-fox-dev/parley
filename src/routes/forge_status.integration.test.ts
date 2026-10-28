import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, EXTERNAL_URL, MOUNT, as, identityService } from '../testutil/app.js';
import { fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What the hub says about how it reaches the forge. The whole point
 * of the route is that it answers when the others cannot, so the
 * thing to prove is that it needs nothing of the forge itself.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

const forge = fakeForge();

async function world() {
  service = await identityService([], { fetch: forge.fetch });
  const owner = await seedUser(service.store);
  for (const [slug, over] of [
    ['mine', { status: 'ready', git_url: 'https://github.com/acme/widgets.git' }],
    ['retired', { status: 'archived', git_url: 'https://github.com/acme/widgets.git' }],
    ['nameless', { status: 'ready', git_url: 'https://code.acme.test/acme/widgets.git' }],
    [
      'carried',
      {
        status: 'ready',
        git_url: 'https://github.com/acme/fork.git',
        workspace_mode: 'carry_patch',
        upstream_url: 'https://gitlab.example.com/group/sub/upstream.git',
        settings: { upstream_forge_api_url: 'https://gitlab.acme.test' },
      },
    ],
  ] as const) {
    await seedWorkspace(service.store, { slug, owner_id: owner.id, ...over });
  }
  await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'a-token');
  await service.secrets.put(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET', 'the-secret');
  await service.secrets.put(workspaceScope('carried'), 'GIT_USERNAME', 'alice');
  await service.secrets.put(workspaceScope('carried'), 'GIT_PASSWORD', 'a-password');

  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const describedBy = async (held: Awaited<ReturnType<typeof world>>, slug: string) => {
  const response = await held.app.request(`${MOUNT}/workspaces/${slug}/forge`, as(held.key));
  expect(response.status, `${slug} answered ${String(response.status)}`).toBe(200);
  return (await response.json()) as Record<string, unknown>;
};

describe('how the hub says it reaches the forge', () => {
  it('describes an ordinary workspace without contacting anything', async () => {
    const held = await world();
    const before = forge.calls().length;

    const described = await describedBy(held, 'mine');

    expect(described).toMatchObject({
      forge: 'github',
      api_url: 'https://api.github.com',
      repository: { host: 'github.com', owner: 'acme', name: 'widgets' },
      credential: 'pat',
      upstream: null,
    });
    expect(forge.calls().length).toBe(before);
  });

  it('describes a host it cannot classify as unknown rather than refusing', async () => {
    const held = await world();

    const described = await describedBy(held, 'nameless');

    expect(described['forge']).toBe('unknown');
    expect(described['api_url']).toBeNull();
    expect(described['repository']).toEqual({
      host: 'code.acme.test',
      owner: 'acme',
      name: 'widgets',
    });
  });

  it('answers for an archived workspace, as the reads do', async () => {
    const held = await world();

    expect((await describedBy(held, 'retired'))['forge']).toBe('github');
  });

  it('describes the upstream beside the origin, each with its own setting', async () => {
    const held = await world();

    const described = await describedBy(held, 'carried');

    expect(described['credential']).toBe('basic');
    expect(described['upstream']).toMatchObject({
      forge: 'gitlab',
      api_url: 'https://gitlab.acme.test/api/v4',
      repository: { host: 'gitlab.example.com', owner: 'group/sub', name: 'upstream' },
      // No upstream secret, so the origin's pair stands in for it.
      credential: 'basic',
    });
  });

  it('reports the webhook block without ever carrying the secret', async () => {
    const held = await world();

    const configured = await describedBy(held, 'mine');
    const bare = await describedBy(held, 'carried');

    expect(configured['webhook']).toEqual({
      configured: true,
      url: `${EXTERNAL_URL}/hooks/forge/mine`,
      last_delivery_at: null,
    });
    expect((bare['webhook'] as { configured: boolean }).configured).toBe(false);
    expect(JSON.stringify(configured)).not.toContain('the-secret');
  });

  it('reports the last delivery the log holds', async () => {
    const { forgeStatusRoutes } = await import('./forge_status.js');
    // Mounted beside the service's own, so the suite can give this
    // one a delivery log while the rest of the service stays real.
    const alt: { store?: unknown; secrets?: unknown } = {};
    service = await identityService(
      forgeStatusRoutes({
        mountPoint: `${MOUNT}/alt`,
        get store() {
          return alt.store as never;
        },
        get secrets() {
          return alt.secrets as never;
        },
        externalUrl: EXTERNAL_URL,
        lastDeliveryAt: async (slug) => (slug === 'mine' ? '2026-10-21T09:12:03.000Z' : undefined),
      }),
      { fetch: forge.fetch },
    );
    alt.store = service.store;
    alt.secrets = service.secrets;
    const owner = await seedUser(service.store);
    await seedWorkspace(service.store, {
      slug: 'mine',
      owner_id: owner.id,
      status: 'ready',
      git_url: 'https://github.com/acme/widgets.git',
    });

    const response = await service.app.request(
      `${MOUNT}/alt/workspaces/mine/forge`,
      as((await seedApiKey(service.store, owner)).presented),
    );
    const body = (await response.json()) as { webhook: { last_delivery_at: string | null } };

    expect(body.webhook.last_delivery_at).toBe('2026-10-21T09:12:03.000Z');
  });

  it('reports no webhook URL where the deployment has no address of its own', async () => {
    const { forgeStatusRoutes } = await import('./forge_status.js');
    const alt: { store?: unknown } = {};
    service = await identityService(
      forgeStatusRoutes({
        mountPoint: `${MOUNT}/alt`,
        get store() {
          return alt.store as never;
        },
      }),
    );
    alt.store = service.store;
    const owner = await seedUser(service.store);
    await seedWorkspace(service.store, {
      slug: 'mine',
      owner_id: owner.id,
      status: 'ready',
      git_url: 'https://github.com/acme/widgets.git',
    });

    const response = await service.app.request(
      `${MOUNT}/alt/workspaces/mine/forge`,
      as((await seedApiKey(service.store, owner)).presented),
    );
    const body = (await response.json()) as { webhook: { url: string | null } };

    expect(body.webhook.url).toBeNull();
  });
});
