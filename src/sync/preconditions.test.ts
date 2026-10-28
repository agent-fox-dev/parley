import { describe, expect, it } from 'vitest';
import type { WorkspaceRecord, WorkspaceStatus } from '../store/workspaces.js';
import { syncRefusal } from './preconditions.js';

/** A workspace record with only what the precondition reads set. */
function workspace(over: Partial<WorkspaceRecord>): WorkspaceRecord {
  return {
    slug: 'a-workspace',
    git_url: 'https://example.test/r.git',
    branch: null,
    owner_id: 'u-1',
    org_id: null,
    status: 'ready',
    display_name: '',
    description: '',
    head_sha: null,
    clone_error: null,
    upstream_head_sha: null,
    last_sync_at: null,
    sync_error: null,
    workspace_mode: 'standard',
    upstream_url: null,
    integration_branch: null,
    settings: {},
    created_at: '2026-03-01T12:00:00Z',
    updated_at: '2026-03-01T12:00:00Z',
    ...over,
  };
}

describe('whether a sync may run', () => {
  it.each(['pending', 'cloning', 'failed', 'archived'])(
    'refuses a workspace that is %s',
    (status) => {
      const refusal = syncRefusal(workspace({ status: status as WorkspaceStatus }));

      expect(refusal?.status).toBe(400);
      expect(refusal?.message).toContain(status);
    },
  );

  it('refuses a ready workspace whose owner switched syncing off', () => {
    const refusal = syncRefusal(workspace({ settings: { sync_enabled: false } }));

    expect(refusal?.status).toBe(400);
    expect(refusal?.message).toContain('switched off');
  });

  it('allows a ready workspace with syncing left at its default', () => {
    expect(syncRefusal(workspace({}))).toBeUndefined();
  });

  it('allows a ready workspace with syncing switched on explicitly', () => {
    expect(syncRefusal(workspace({ settings: { sync_enabled: true } }))).toBeUndefined();
  });
});
