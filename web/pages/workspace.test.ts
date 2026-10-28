import { describe, expect, it } from 'vitest';
import { renderWorkspace } from '../render.js';

/**
 * What one workspace's page says, section by section. Each section
 * renders on its own, so a read that failed takes out its own
 * section and nothing else.
 */
const WORKSPACE = {
  slug: 'widgets',
  display_name: 'Widgets',
  description: 'the widget service',
  status: 'ready',
  workspace_mode: 'standard',
  head_sha: 'a'.repeat(40),
  upstream_head_sha: null,
  last_sync_at: '2026-10-21T09:12:03.000Z',
  clone_error: null,
  sync_error: null,
  settings: { sync_enabled: true, rebuild_strategy: 'rebase' },
};

const sections = (over: Record<string, unknown> = {}) => ({
  workspace: WORKSPACE as Record<string, unknown>,
  ...over,
});

describe('a workspace’s own fields', () => {
  it('are shown with the values the API gave', () => {
    const html = renderWorkspace('alice', sections());

    for (const shown of [
      'widgets',
      'Widgets',
      'the widget service',
      'ready',
      'standard',
      'a'.repeat(40),
      '2026-10-21 09:12:03Z',
      'sync_enabled',
      'rebuild_strategy',
    ]) {
      expect(html, shown).toContain(shown);
    }
  });

  it('show an error the workspace has, and no error field it has not', () => {
    const failed = renderWorkspace(
      'alice',
      sections({
        workspace: { ...WORKSPACE, status: 'error', clone_error: 'the remote refused the clone' },
      }),
    );
    const well = renderWorkspace('alice', sections());

    expect(failed).toContain('the remote refused the clone');
    expect(failed).toMatch(/clone error/i);
    expect(well).not.toMatch(/clone error/i);
    expect(well).not.toMatch(/sync error/i);
  });

  it('say a workspace has never synced rather than showing a gap', () => {
    const html = renderWorkspace(
      'alice',
      sections({ workspace: { ...WORKSPACE, last_sync_at: null } }),
    );

    expect(html).toContain('never');
  });
});

describe('a carry-patch workspace', () => {
  const carried = {
    workspace: { ...WORKSPACE, workspace_mode: 'carry_patch' },
    patchStatus: {
      patches: [
        { position: 2, branch_name: 'second', status: 'conflict', last_rebuild_result: 'conflict' },
        { position: 1, branch_name: 'first', status: 'active', last_rebuild_result: 'success' },
      ],
      last_rebuild: { id: 'r-1', status: 'completed' },
    },
  };

  it('shows its patches in position order with their latest rebuild result', () => {
    const html = renderWorkspace('alice', carried);

    expect(html).toMatch(/Patches/);
    expect(html.indexOf('first')).toBeLessThan(html.indexOf('second'));
    expect(html).toContain('conflict');
    expect(html).toContain('success');
  });

  it('shows the latest rebuild, or says none has run', () => {
    const withRebuild = renderWorkspace('alice', carried);
    const without = renderWorkspace('alice', {
      ...carried,
      patchStatus: { patches: [], last_rebuild: null },
    });

    expect(withRebuild).toMatch(/completed/);
    expect(without).toMatch(/no rebuild has run/i);
  });

  it('is the only kind of workspace with a patch section', () => {
    const standard = renderWorkspace('alice', sections());

    expect(standard).not.toMatch(/Patches/);
  });
});

describe('the recent jobs and events', () => {
  it('are shown newest first, each with its kind and time', () => {
    const html = renderWorkspace('alice', {
      ...sections(),
      jobs: [
        { kind: 'merge', status: 'completed', at: '2026-10-21T10:00:00.000Z' },
        { kind: 'rebuild', status: 'failed', at: '2026-10-21T09:00:00.000Z' },
      ],
      events: [
        { event_type: 'hub.workspace.sync', timestamp: '2026-10-21T11:00:00.000Z' },
        { event_type: 'hub.git.push', timestamp: '2026-10-21T10:30:00.000Z' },
      ],
    });

    expect(html.indexOf('merge — completed')).toBeLessThan(html.indexOf('rebuild — failed'));
    expect(html.indexOf('hub.workspace.sync')).toBeLessThan(html.indexOf('hub.git.push'));
    expect(html).toContain('2026-10-21 11:00:00Z');
  });

  it('say so where there are none', () => {
    const html = renderWorkspace('alice', { ...sections(), jobs: [], events: [] });

    expect(html).toMatch(/no jobs/i);
    expect(html).toMatch(/no events/i);
  });
});

describe('a section that could not be read', () => {
  it('says which one, and leaves the others rendered', () => {
    const html = renderWorkspace('alice', {
      ...sections(),
      jobs: undefined,
      events: [{ event_type: 'hub.workspace.sync', timestamp: '2026-10-21T11:00:00.000Z' }],
    });

    expect(html).toMatch(/jobs could not be read/i);
    // The rest of the page is still there.
    expect(html).toContain('widgets');
    expect(html).toContain('hub.workspace.sync');
  });
});
