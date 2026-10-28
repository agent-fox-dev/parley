import { afterEach, describe, expect, it } from 'vitest';
import { type Service, identityService } from '../../src/testutil/app.js';
import { SEED_NOW, seedPatch, seedUser, seedWorkspace } from '../../src/testutil/identity.js';
import { insertRebuild } from '../../src/rebuild/record.js';
import { eventRecorder } from '../../src/audit/recorder.js';
import { SESSION_COOKIE } from '../session.js';

/**
 * One workspace's page against a deployment that really holds one:
 * the fields come from the API, the patch stack from the patch
 * status, the jobs from the job lists and the events from the audit
 * query — and a section whose read fails costs itself alone.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService([], { clock: 'ticking' });
  const { store } = service;
  const alice = await seedUser(store, { username: 'alice', provider_id: '12345' });

  await seedWorkspace(store, {
    slug: 'widgets',
    owner_id: alice.id,
    status: 'ready',
    description: 'the widget service',
    head_sha: 'a'.repeat(40),
    last_sync_at: '2026-10-21T09:12:03.000Z',
    settings: { sync_enabled: true },
  });
  await seedWorkspace(store, {
    slug: 'carried',
    owner_id: alice.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    upstream_url: 'https://github.com/acme/upstream.git',
    integration_branch: 'integration',
  });
  await seedWorkspace(store, {
    slug: 'broken',
    owner_id: alice.id,
    status: 'failed',
    clone_error: 'the remote refused the clone',
  });

  for (const [index, branch] of ['first', 'second', 'third'].entries()) {
    await seedPatch(store, 'carried', branch, { position: index + 1 });
  }
  await insertRebuild(
    store,
    {
      id: 'r-1',
      workspace_slug: 'carried',
      strategy: 'rebase',
      fail_mode: 'fail_fast',
      submitted_by: alice.id,
    },
    SEED_NOW.toISOString(),
  );

  // Something to show in the events section.
  const recorder = eventRecorder({ store, clock: { now: () => SEED_NOW } });
  for (const slug of ['widgets', 'carried']) {
    await recorder.record({
      type: 'hub.workspace.sync',
      workspace: slug,
      resourceId: slug,
      metadata: { result: 'fast_forward' },
    });
  }

  return service;
}

async function sessionFor(held: Service): Promise<string> {
  const response = await held.app.request('/signin/github/callback?code=a-code');
  return /parley_session=([^;]+)/.exec(response.headers.get('set-cookie') ?? '')?.[1] ?? '';
}

const withSession = (value: string) => ({ headers: { cookie: `${SESSION_COOKIE}=${value}` } });

async function view(held: Service, slug: string): Promise<string> {
  const session = await sessionFor(held);
  const response = await held.app.request(`/workspaces/${slug}`, withSession(session));
  expect(response.status, `${slug} answered ${String(response.status)}`).toBe(200);
  return response.text();
}

describe('a workspace’s page', () => {
  it('shows the fields the API returns for it', async () => {
    const held = await world();

    const html = await view(held, 'widgets');

    expect(html).toContain('widgets');
    expect(html).toContain('the widget service');
    expect(html).toContain('ready');
    expect(html).toContain('a'.repeat(40));
    expect(html).toContain('2026-10-21 09:12:03Z');
    expect(html).toContain('sync_enabled');
  });

  it('shows an error the workspace carries, and none it does not', async () => {
    const held = await world();

    const broken = await view(held, 'broken');
    const well = await view(held, 'widgets');

    expect(broken).toContain('the remote refused the clone');
    expect(well).not.toMatch(/clone error/i);
  });

  it('shows a carry-patch workspace’s patches in order, and its rebuild', async () => {
    const held = await world();

    const html = await view(held, 'carried');

    expect(html).toMatch(/Patches/);
    expect(html.indexOf('first')).toBeLessThan(html.indexOf('second'));
    expect(html.indexOf('second')).toBeLessThan(html.indexOf('third'));
    expect(html).toContain('r-1');
  });

  it('shows no patch section for a workspace that carries none', async () => {
    const held = await world();

    expect(await view(held, 'widgets')).not.toMatch(/Patches/);
  });

  it('shows the recent events the audit query returns', async () => {
    const held = await world();

    const html = await view(held, 'widgets');

    expect(html).toContain('hub.workspace.sync');
    // And only this workspace's.
    expect([...html.matchAll(/hub\.workspace\.sync/g)]).toHaveLength(1);
  });

  it('is served in full when one section’s read fails', async () => {
    const held = await world();
    const session = await sessionFor(held);

    // The audit table is gone, so the events read fails and nothing
    // else does.
    await held.store.run('DROP TABLE hub_events');
    const response = await held.app.request('/workspaces/widgets', withSession(session));
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toMatch(/events could not be read/i);
    expect(html).toContain('widgets');
    expect(html).toContain('sync_enabled');
    expect(html).toMatch(/Recent jobs/);
  });
});
