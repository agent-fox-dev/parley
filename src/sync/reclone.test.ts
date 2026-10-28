import { describe, expect, it } from 'vitest';
import type { WorkspaceRecord, WorkspaceStatus } from '../store/workspaces.js';
import { recloneRefusal } from './reclone.js';

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

describe('whether a reclone may run', () => {
  it('refuses an archived workspace, and says what to do instead', () => {
    const refusal = recloneRefusal(workspace({ status: 'archived' }));

    expect(refusal?.status).toBe(400);
    expect(refusal?.message).toContain('reactivate');
  });

  it.each(['pending', 'cloning'])('refuses one whose copy is already under way: %s', (status) => {
    const refusal = recloneRefusal(workspace({ status: status as WorkspaceStatus }));

    expect(refusal?.status).toBe(409);
  });

  it.each(['ready', 'failed'])('allows one that is %s, which is what it is for', (status) => {
    expect(recloneRefusal(workspace({ status: status as WorkspaceStatus }))).toBeUndefined();
  });
});
