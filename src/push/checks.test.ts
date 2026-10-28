import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import {
  branchNameRefusal,
  inRepositoryRefusal,
  readPushBody,
  readinessRefusal,
  unpushableRefusal,
} from './checks.js';

function workspace(over: Partial<WorkspaceRecord> = {}): WorkspaceRecord {
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

describe('the body a branch push may carry', () => {
  it.each([
    ['nothing at all', undefined],
    ['an empty object', {}],
    ['fields nobody reads', { whatever: 'is here' }],
    ['force false', { force: false }],
  ])('pushes without force for %s', (_what, body) => {
    expect(readPushBody(body)).toEqual({ force: false });
  });

  it('pushes with force when it is asked for', () => {
    expect(readPushBody({ force: true })).toEqual({ force: true });
  });

  it.each([
    ['a string', 'force'],
    ['a number', 7],
    ['an array', [{ force: true }]],
  ])('refuses a body that is %s', (_what, body) => {
    expect(() => readPushBody(body)).toThrow(HttpError);
  });

  it.each([
    ['a string', { force: 'yes' }],
    ['a number', { force: 1 }],
    ['null', { force: null }],
  ])('refuses a force that is %s', (_what, body) => {
    expect(() => readPushBody(body)).toThrow(/force must be true or false/);
  });
});

describe('a branch name the route is given', () => {
  it.each([
    ['one starting with a hyphen', '-x', "must not start with '-'"],
    ['one holding two dots', 'a..b', "must not contain '..'"],
    ['one holding a control character', 'a\u0001b', 'control character'],
    ['one ending in .lock', 'feature.lock', "must not end with '.lock'"],
    ['an empty one', '', 'must not be empty'],
    ['one holding a space', 'a branch', 'space'],
    ['one holding @{', 'a@{0}', "must not contain '@{'"],
  ])('is refused when it is %s, naming the rule', (_what, name, rule) => {
    const refused = branchNameRefusal(name);

    expect(refused?.status).toBe(404);
    expect(refused?.errorType).toBe('branch_not_found');
    expect(refused?.message.startsWith('branch_not_found: ')).toBe(true);
    expect(refused?.message).toContain(rule);
  });

  it.each(['main', 'fix/42-nil-map', 'a%25b', 'release-1.2', 'feature/a/b'])(
    'is accepted when git would accept it: %s',
    (name) => {
      expect(branchNameRefusal(name)).toBeUndefined();
    },
  );
});

describe('a workspace that is not ready', () => {
  it.each(['pending', 'cloning', 'failed', 'archived'])('refuses a push: %s', (status) => {
    const refused = readinessRefusal(workspace({ status: status as 'pending' }));

    expect(refused?.status).toBe(400);
    expect(refused?.message).toContain('ready');
  });

  it('allows one that is ready', () => {
    expect(readinessRefusal(workspace())).toBeUndefined();
  });
});

describe('the branches this route will not send', () => {
  it('refuses the integration branch of a carry-patch workspace', () => {
    const refused = unpushableRefusal(
      workspace({ workspace_mode: 'carry_patch', integration_branch: 'integration' }),
      'integration',
      [],
      false,
    );

    expect(refused?.status).toBe(400);
    expect(refused?.errorType).toBe('integration_branch');
    expect(refused?.message.startsWith('integration_branch: ')).toBe(true);
  });

  it('refuses a registered patch branch', () => {
    const refused = unpushableRefusal(
      workspace({ workspace_mode: 'carry_patch', integration_branch: 'integration' }),
      'a-patch',
      ['a-patch'],
      false,
    );

    expect(refused?.errorType).toBe('patch_branch');
  });

  it("refuses the workspace's own branch when force is given, and allows it without", () => {
    const place = workspace({ branch: 'main' });

    expect(unpushableRefusal(place, 'main', [], true)?.errorType).toBe('protected_branch');
    expect(unpushableRefusal(place, 'main', [], false)).toBeUndefined();
  });

  it('allows an ordinary branch, and a patch branch on a standard workspace', () => {
    expect(unpushableRefusal(workspace(), 'feature', [], true)).toBeUndefined();
    expect(unpushableRefusal(workspace(), 'a-patch', ['a-patch'], false)).toBeUndefined();
  });
});

describe('the protections that need the repository', () => {
  it('refuses a forced push of the branch HEAD follows where none is named', () => {
    const refused = inRepositoryRefusal({
      workspace: workspace({ branch: null }),
      name: 'main',
      force: true,
      head: 'main',
      present: true,
    });

    expect(refused?.errorType).toBe('protected_branch');
    expect(refused?.message).toContain('HEAD');
  });

  it('allows that same push without force', () => {
    expect(
      inRepositoryRefusal({
        workspace: workspace({ branch: null }),
        name: 'main',
        force: false,
        head: 'main',
        present: true,
      }),
    ).toBeUndefined();
  });

  it.each([true, false])('refuses a branch the repository lacks, force %s', (force) => {
    const refused = inRepositoryRefusal({
      workspace: workspace(),
      name: 'nowhere',
      force,
      head: 'main',
      present: false,
    });

    expect(refused?.status).toBe(404);
    expect(refused?.errorType).toBe('branch_not_found');
    expect(refused?.message).toContain('nowhere');
  });
});
