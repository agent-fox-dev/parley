import { describe, expect, it } from 'vitest';
import { renderDashboard } from '../render.js';

/**
 * What the dashboard shows when there is nothing to show, and what
 * each row of it says when there is.
 */
describe('a dashboard with no workspaces', () => {
  it('says there are none and what to do next', () => {
    const html = renderDashboard('alice', []);

    expect(html).toMatch(/no workspaces yet/i);
    expect(html).toContain('afc workspace create');
    // Not a bare list with nothing in it.
    expect(html).toContain('class="empty"');
  });
});

describe('a dashboard that could not be read', () => {
  it('says so where the list would be, rather than showing none', () => {
    const html = renderDashboard('alice', undefined);

    expect(html).toMatch(/could not be read/i);
    expect(html).not.toMatch(/no workspaces yet/i);
  });
});

describe('a row of the dashboard', () => {
  const workspace = {
    slug: 'widgets',
    status: 'ready',
    access: 'owner',
    last_sync_at: '2026-10-21T09:12:03.000Z',
  };

  it('carries the slug, the status, the access and the last sync', () => {
    const html = renderDashboard('alice', [workspace]);

    expect(html).toContain('widgets');
    expect(html).toContain('ready');
    expect(html).toContain('owner');
    expect(html).toContain('2026-10-21 09:12:03Z');
  });

  it('links to that workspace’s view', () => {
    const html = renderDashboard('alice', [workspace]);

    expect(html).toContain('href="/workspaces/widgets"');
  });

  it('says a never-synced workspace has never synced', () => {
    const html = renderDashboard('alice', [{ ...workspace, last_sync_at: null }]);

    expect(html).toContain('synced never');
  });

  it('shows a status in the colour its state earns', () => {
    const html = renderDashboard('alice', [
      { ...workspace, status: 'ready' },
      { ...workspace, slug: 'other', status: 'cloning' },
      { ...workspace, slug: 'broken', status: 'error' },
    ]);

    expect(html).toContain('badge badge-success');
    expect(html).toContain('badge badge-attention');
    expect(html).toContain('badge badge-danger');
  });
});
