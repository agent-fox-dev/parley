import { afterEach, describe, expect, it } from 'vitest';
import { type Service, identityService } from '../../src/testutil/app.js';
import { seedOrg, seedUser, seedWorkspace } from '../../src/testutil/identity.js';

/**
 * What the interface tells somebody who has not signed in. The
 * answer is: what the service is, and nothing whatever about what
 * this deployment holds — not a slug, not a name, not a count.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

const SECRETS = {
  workspace: 'a-workspace-nobody-should-see',
  user: 'a-person-nobody-should-see',
  org: 'an-organization-nobody-should-see',
};

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store, { username: SECRETS.user });
  const org = await seedOrg(service.store, owner, { name: SECRETS.org, slug: SECRETS.org });
  await seedWorkspace(service.store, {
    slug: SECRETS.workspace,
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  return service;
}

const WITHOUT_A_SESSION = [
  '/',
  '/dashboard',
  `/workspaces/${SECRETS.workspace}`,
  '/workspaces/nothing-here',
];

describe('a visitor who has not signed in', () => {
  it.each(WITHOUT_A_SESSION)('is shown the landing page at %s', async (path) => {
    const held = await world();

    const response = await held.app.request(path);
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(response.headers.get('content-type')).toContain('text/html');
    expect(html).toContain('Sign in');
    expect(html).toMatch(/Parley/);
  });

  it.each(WITHOUT_A_SESSION)(
    'is told nothing of what this deployment holds at %s',
    async (path) => {
      const held = await world();

      const html = await (await held.app.request(path)).text();

      for (const [what, name] of Object.entries(SECRETS)) {
        expect(html, `the ${what} name`).not.toContain(name);
      }
      // Nor how many of anything there are.
      expect(html).not.toMatch(/\b1 workspace\b/);
    },
  );
});
