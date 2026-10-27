import { describe, expect, it } from 'vitest';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { WORKSPACE_SETTINGS } from './settings.js';
import { serialiseWorkspace } from './serialise.js';

const WORKSPACE_KEYS = [
  'access',
  'branch',
  'clone_error',
  'created_at',
  'description',
  'display_name',
  'git_url',
  'head_sha',
  'hub_url',
  'integration_branch',
  'last_sync_at',
  'org_id',
  'org_slug',
  'owner_id',
  'settings',
  'slug',
  'status',
  'sync_error',
  'updated_at',
  'upstream_head_sha',
  'upstream_url',
  'workspace_mode',
];

function record(over: Partial<WorkspaceRecord> = {}): WorkspaceRecord {
  return {
    slug: 'my-project',
    git_url: 'https://github.com/u/r.git',
    branch: null,
    owner_id: 'user-1',
    org_id: 'org-1',
    status: 'ready',
    display_name: 'My Project',
    description: 'a description',
    head_sha: null,
    clone_error: null,
    upstream_head_sha: null,
    last_sync_at: null,
    sync_error: null,
    workspace_mode: 'standard',
    upstream_url: null,
    integration_branch: null,
    settings: {},
    created_at: '2026-03-01T12:00:00.000Z',
    updated_at: '2026-03-01T12:00:00.000Z',
    ...over,
  };
}

const context = { access: 'owner' as const, orgSlug: 'an-org', externalUrl: 'https://parley.test' };

describe('a serialised workspace', () => {
  it('carries the documented keys and no others', () => {
    const serialised = serialiseWorkspace(record(), context);

    expect(Object.keys(serialised).toSorted()).toEqual(WORKSPACE_KEYS);
  });

  it('carries the effective value of every setting', () => {
    const serialised = serialiseWorkspace(record({ settings: { sync_enabled: false } }), context);

    expect(Object.keys(serialised.settings).toSorted()).toEqual(
      Object.keys(WORKSPACE_SETTINGS).toSorted(),
    );
    expect(serialised.settings['sync_enabled']).toBe(false);
    expect(serialised.settings['rebuild_strategy']).toBe('rebase');
  });

  it.each([
    ['a display_name that was never set', { display_name: '' }, 'my-project'],
    ['a display_name that was set', { display_name: 'My Project' }, 'My Project'],
  ])('never answers with an empty display_name: %s', (_name, over, expected) => {
    expect(serialiseWorkspace(record(over), context).display_name).toBe(expected);
  });

  it('answers with an empty description rather than null', () => {
    expect(serialiseWorkspace(record({ description: '' }), context).description).toBe('');
  });
});

describe('the clone URL a workspace advertises', () => {
  it('names the organization and the slug when the deployment has an external URL', () => {
    expect(serialiseWorkspace(record(), context).hub_url).toBe(
      'https://parley.test/git/an-org/my-project.git',
    );
  });

  it('does not double the separator when the external URL ends in one', () => {
    expect(
      serialiseWorkspace(record(), { ...context, externalUrl: 'https://parley.test/' }).hub_url,
    ).toBe('https://parley.test/git/an-org/my-project.git');
  });

  it.each([
    ['the deployment has no external URL', { externalUrl: undefined }],
    ['the deployment configured an empty external URL', { externalUrl: '' }],
    ['the workspace has no organization', { orgSlug: null }],
  ])('is null when %s', (_name, over) => {
    expect(serialiseWorkspace(record(), { ...context, ...over }).hub_url).toBeNull();
  });
});
