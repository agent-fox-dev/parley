import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { type FakeForge, fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * The nine reads, against a stub of each forge. What matters is that
 * two quite different payloads come back as one documented shape, so
 * every case asserts the shape rather than the forge's own words.
 */
let service: Service;
let forge: FakeForge;

afterEach(() => {
  service.dispose();
});

/** What GitHub answers, in its own shapes. */
const GITHUB = {
  'GET /repos/acme/widgets': {
    body: {
      full_name: 'acme/widgets',
      default_branch: 'main',
      private: true,
      archived: false,
      permissions: { push: true, pull: true, admin: false },
      allow_squash_merge: true,
    },
  },
  'GET /repos/acme/widgets/issues': {
    body: [
      {
        number: 12,
        title: 'Widgets break',
        body: 'it breaks',
        state: 'open',
        html_url: 'https://github.com/acme/widgets/issues/12',
        user: { login: 'alice' },
        labels: [{ name: 'af:triage' }],
        created_at: '2026-01-02T03:04:05Z',
        updated_at: '2026-01-02T03:04:05Z',
      },
    ],
  },
  'GET /repos/acme/widgets/issues/12': {
    body: {
      number: 12,
      title: 'Widgets break',
      body: 'it breaks',
      state: 'open',
      user: { login: 'alice' },
      labels: [{ name: 'af:triage' }],
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
      pull_request: { url: 'https://api.github.com/repos/acme/widgets/pulls/12' },
    },
  },
  'GET /repos/acme/widgets/issues/12/comments': {
    body: [
      {
        id: 7,
        body: 'a comment',
        user: { login: 'bob' },
        created_at: '2026-01-02T04:00:00Z',
        html_url: 'https://github.com/acme/widgets/issues/12#issuecomment-7',
      },
    ],
  },
  'GET /repos/acme/widgets/pulls/5': {
    body: {
      number: 5,
      title: 'Fix it',
      body: 'the fix',
      state: 'open',
      draft: true,
      merged: false,
      head: { sha: 'abc123', ref: 'feature' },
      base: { ref: 'main' },
      user: { login: 'alice' },
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
    },
  },
  'GET /repos/acme/widgets/pulls/5/files': {
    body: [{ filename: 'a.txt', status: 'modified', additions: 3, deletions: 1 }],
  },
  'GET /repos/acme/widgets/commits/abc123/check-runs': {
    body: {
      check_runs: [
        {
          name: 'build',
          status: 'completed',
          conclusion: 'success',
          html_url: 'https://ci.test/1',
        },
      ],
    },
  },
  'GET /repos/acme/widgets/pulls/5/reviews': {
    body: [
      {
        user: { login: 'carol' },
        state: 'APPROVED',
        body: 'looks right',
        submitted_at: '2026-01-02T05:00:00Z',
      },
    ],
  },
};

/** What GitLab answers, in its own shapes. */
const GITLAB = {
  'GET /api/v4/projects/group%2Fwidgets': {
    body: {
      path_with_namespace: 'group/widgets',
      default_branch: 'main',
      visibility: 'private',
      archived: false,
      permissions: { project_access: { access_level: 40 } },
    },
  },
  'GET /api/v4/projects/group%2Fwidgets/issues': {
    body: [
      {
        iid: 12,
        title: 'Widgets break',
        description: 'it breaks',
        state: 'opened',
        web_url: 'https://gitlab.test/group/widgets/-/issues/12',
        author: { username: 'alice' },
        labels: ['af:triage'],
        created_at: '2026-01-02T03:04:05Z',
        updated_at: '2026-01-02T03:04:05Z',
      },
    ],
  },
  'GET /api/v4/projects/group%2Fwidgets/issues/12': {
    body: {
      iid: 12,
      title: 'Widgets break',
      description: 'it breaks',
      state: 'opened',
      author: { username: 'alice' },
      labels: ['af:triage'],
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
    },
  },
  'GET /api/v4/projects/group%2Fwidgets/issues/12/notes': {
    body: [
      {
        id: 7,
        body: 'a comment',
        author: { username: 'bob' },
        created_at: '2026-01-02T04:00:00Z',
      },
    ],
  },
  'GET /api/v4/projects/group%2Fwidgets/merge_requests/5': {
    body: {
      iid: 5,
      title: 'Fix it',
      description: 'the fix',
      state: 'opened',
      draft: true,
      sha: 'abc123',
      source_branch: 'feature',
      target_branch: 'main',
      author: { username: 'alice' },
      created_at: '2026-01-02T03:04:05Z',
      updated_at: '2026-01-02T03:04:05Z',
    },
  },
  'GET /api/v4/projects/group%2Fwidgets/merge_requests/5/changes': {
    body: {
      changes: [
        {
          new_path: 'a.txt',
          old_path: 'a.txt',
          new_file: false,
          deleted_file: false,
          diff: '@@\n+one\n-two\n',
        },
      ],
    },
  },
  'GET /api/v4/projects/group%2Fwidgets/merge_requests/5/pipelines': {
    body: [
      { id: 3, status: 'success', web_url: 'https://gitlab.test/pipelines/3', ref: 'feature' },
    ],
  },
  'GET /api/v4/projects/group%2Fwidgets/merge_requests/5/approvals': {
    body: {
      approved_by: [{ user: { username: 'carol' } }],
      approvals_left: 0,
    },
  },
};

async function world(which: 'github' | 'gitlab') {
  forge = fakeForge({ answers: which === 'github' ? GITHUB : GITLAB });
  service = await identityService([], { fetch: forge.fetch });
  const owner = await seedUser(service.store);
  const gitUrl =
    which === 'github'
      ? 'https://github.com/acme/widgets.git'
      : 'https://gitlab.test/group/widgets.git';
  for (const [slug, status] of [
    ['mine', 'ready'],
    ['retired', 'archived'],
  ] as const) {
    await seedWorkspace(service.store, { slug, owner_id: owner.id, status, git_url: gitUrl });
  }
  await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', 'a-token');
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const read = async (held: Awaited<ReturnType<typeof world>>, path: string, slug = 'mine') => {
  const response = await held.app.request(`${MOUNT}/workspaces/${slug}/${path}`, as(held.key));
  expect(response.status, `${path} answered ${String(response.status)}`).toBe(200);
  return response.json() as Promise<never>;
};

describe.each(['github', 'gitlab'] as const)('reading from %s', (which) => {
  it('answers the repository in the documented shape', async () => {
    const held = await world(which);

    const repository = (await read(held, 'forge/repository')) as Record<string, unknown>;

    expect(repository['full_name']).toBe(which === 'github' ? 'acme/widgets' : 'group/widgets');
    expect(repository['default_branch']).toBe('main');
    expect(repository['private']).toBe(true);
    expect(repository['archived']).toBe(false);
    expect(repository['permissions']).toMatchObject({ push: true, pull: true });
  });

  it('answers an issue list as a bare array of issues', async () => {
    const held = await world(which);

    const issues = (await read(held, 'issues')) as Array<Record<string, unknown>>;

    expect(Array.isArray(issues)).toBe(true);
    expect(issues[0]).toMatchObject({
      number: 12,
      title: 'Widgets break',
      body: 'it breaks',
      state: 'open',
      author: { login: 'alice' },
      labels: ['af:triage'],
      is_pr: false,
    });
  });

  it('answers an issue thread with its comments', async () => {
    const held = await world(which);

    const thread = (await read(held, 'issues/12')) as {
      issue: Record<string, unknown>;
      comments: Array<Record<string, unknown>>;
      truncated: boolean;
    };

    expect(thread.issue['number']).toBe(12);
    expect(thread.comments[0]).toMatchObject({ body: 'a comment', user: { login: 'bob' } });
    expect(thread.truncated).toBe(false);
  });

  it('answers the comments alone as a bare array', async () => {
    const held = await world(which);

    const comments = (await read(held, 'issues/12/comments')) as Array<Record<string, unknown>>;

    expect(Array.isArray(comments)).toBe(true);
    expect(comments[0]?.['body']).toBe('a comment');
  });

  it('answers a pull request and its state', async () => {
    const held = await world(which);

    const pull = (await read(held, 'pulls/5')) as Record<string, unknown>;
    const state = (await read(held, 'pulls/5/state')) as Record<string, unknown>;

    expect(pull).toMatchObject({
      number: 5,
      title: 'Fix it',
      state: 'open',
      draft: true,
      merged: false,
      head_sha: 'abc123',
      head_branch: 'feature',
      base_branch: 'main',
    });
    expect(state).toEqual({ number: 5, state: 'open', merged: false, head_sha: 'abc123' });
  });

  it('answers the changed files, the checks and the reviews', async () => {
    const held = await world(which);

    const files = (await read(held, 'pulls/5/files')) as Array<Record<string, unknown>>;
    const checks = (await read(held, 'pulls/5/checks')) as Array<Record<string, unknown>>;
    const reviews = (await read(held, 'pulls/5/reviews')) as Array<Record<string, unknown>>;

    expect(files[0]?.['filename']).toBe('a.txt');
    expect(checks[0]).toMatchObject({ status: expect.any(String) });
    expect(reviews[0]?.['author']).toBe('carol');
  });

  it('serves an archived workspace', async () => {
    const held = await world(which);

    expect(
      (await held.app.request(`${MOUNT}/workspaces/retired/issues`, as(held.key))).status,
    ).toBe(200);
  });

  it('answers an empty list as an empty array rather than null', async () => {
    const held = await world(which);
    forge.answerWith({});

    const issues = (await read(held, 'issues')) as unknown[];

    expect(issues).toEqual([]);
  });
});

describe('an issue thread whose comments could not be read', () => {
  it('answers 200 with the issue and the comments’ error, carrying no token', async () => {
    const held = await world('github');
    forge.answerWith({
      ...GITHUB,
      'GET /repos/acme/widgets/issues/12/comments': {
        status: 403,
        body: { message: 'bad token a-token at https://user:pass@github.com/acme/widgets' },
      },
    });

    const response = await held.app.request(`${MOUNT}/workspaces/mine/issues/12`, as(held.key));
    const text = await response.text();
    const thread = JSON.parse(text) as {
      issue: Record<string, unknown>;
      comments: unknown[];
      comments_err?: string;
    };

    expect(response.status).toBe(200);
    expect(thread.issue['number']).toBe(12);
    expect(thread.comments).toEqual([]);
    expect(thread.comments_err).toMatch(/\S/);
    expect(text).not.toContain('a-token');
    expect(text).not.toContain('user:pass');
  });

  it('says a thread is truncated when more comments exist than were read', async () => {
    const held = await world('github');
    forge.answerWith({
      ...GITHUB,
      'GET /repos/acme/widgets/issues/12/comments': {
        body: Array.from({ length: 100 }, (_, index) => ({
          id: index,
          body: `comment ${String(index)}`,
          user: { login: 'bob' },
          created_at: '2026-01-02T04:00:00Z',
        })),
        headers: { link: '<https://api.github.com/x?page=2>; rel="next"' },
      },
    });

    const thread = (await read(held, 'issues/12')) as { truncated: boolean; comments: unknown[] };

    expect(thread.comments).toHaveLength(100);
    expect(thread.truncated).toBe(true);
  });
});
