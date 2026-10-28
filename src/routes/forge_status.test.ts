import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What the description route refuses. It is the route an operator
 * runs when another answers forge_unknown, so its own refusals have
 * to say what is wrong with the configuration without quoting back a
 * value that may hold a credential.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world(settings: Record<string, unknown> = {}) {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://github.com/acme/widgets.git',
    settings,
  });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

describe('an API URL setting the service will not use', () => {
  it.each([
    ['one that is not absolute', 'api.github.com'],
    ['one that is not https', 'http://github.acme.test/api/v3'],
    ['one carrying userinfo', 'https://token@github.acme.test/api/v3'],
    ['one carrying a query', 'https://github.acme.test/api/v3?token=secret'],
    ['one carrying a fragment', 'https://github.acme.test/api/v3#here'],
  ])('refuses %s, naming the setting and the rule but not the value', async (_what, setting) => {
    const held = await world({ forge_api_url: setting });

    const response = await held.app.request(`${MOUNT}/workspaces/mine/forge`, as(held.key));
    const body = (await response.json()) as { error: { message: string; error_type: string } };

    expect(response.status).toBe(422);
    expect(body.error.error_type).toBe('forge_api_url_invalid');
    expect(body.error.message).toContain('forge_api_url');
    expect(body.error.message).not.toContain(setting);
  });

  it('names the upstream setting when it is the upstream’s that is wrong', async () => {
    service = await identityService();
    const owner = await seedUser(service.store);
    await seedWorkspace(service.store, {
      slug: 'carried',
      owner_id: owner.id,
      status: 'ready',
      git_url: 'https://github.com/acme/widgets.git',
      workspace_mode: 'carry_patch',
      upstream_url: 'https://github.com/acme/upstream.git',
      settings: { upstream_forge_api_url: 'http://not-https.test' },
    });
    const key = (await seedApiKey(service.store, owner)).presented;

    const response = await service.app.request(`${MOUNT}/workspaces/carried/forge`, as(key));
    const body = (await response.json()) as { error: { message: string; error_type: string } };

    expect(response.status).toBe(422);
    expect(body.error.message).toContain('upstream_forge_api_url');
  });
});

describe('a store that cannot be read', () => {
  it('answers 500 rather than a description missing its webhook block', async () => {
    const { forgeStatusRoutes } = await import('./forge_status.js');
    // The secret store answers every read with a failure.
    const broken = {
      get: () => Promise.reject(new Error('the store is down')),
      put: () => Promise.reject(new Error('the store is down')),
      delete: () => Promise.reject(new Error('the store is down')),
      list: () => Promise.reject(new Error('the store is down')),
    };
    const alt: { store?: unknown } = {};
    service = await identityService(
      forgeStatusRoutes({
        mountPoint: `${MOUNT}/alt`,
        get store() {
          return alt.store as never;
        },
        secrets: broken as never,
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

    expect(response.status).toBe(500);
    expect(await response.text()).not.toContain('webhook');
  });
});
