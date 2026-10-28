import { describe, expect, it } from 'vitest';
import {
  apiKey,
  asJson,
  baseUrl,
  bearer,
  call,
  missing,
  needed,
  uniqueSlug,
} from '../testutil/smoke.js';

/**
 * Registration, failure and retirement against a deployment with a real
 * repository store and a real remote. Each suite names what it needs.
 */
const gitUrl = process.env['PARLEY_SMOKE_GIT_URL'] ?? '';
const gitToken = process.env['PARLEY_SMOKE_GIT_TOKEN'] ?? '';

const noRemote = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_GIT_URL', gitUrl],
);
const noPrivate = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_GIT_URL', gitUrl],
  ['PARLEY_SMOKE_GIT_TOKEN', gitToken],
);

interface Workspace {
  slug: string;
  status: string;
  head_sha: string | null;
  clone_error: string | null;
}

async function read(slug: string): Promise<Workspace> {
  const response = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey));
  expect(response.status).toBe(200);
  return (await response.json()) as Workspace;
}

/** Waits for a copy to settle, because it runs outside the request. */
async function settled(slug: string, within = 120_000): Promise<Workspace> {
  const deadline = Date.now() + within;
  let workspace = await read(slug);
  while (
    Date.now() < deadline &&
    (workspace.status === 'pending' || workspace.status === 'cloning')
  ) {
    await new Promise((resolve) => setTimeout(resolve, 2000));
    workspace = await read(slug);
  }
  return workspace;
}

async function retire(slug: string): Promise<void> {
  await call(`/api/v1/workspaces/${slug}/archive`, bearer(apiKey, { method: 'POST' }));
  await call(`/api/v1/workspaces/${slug}`, bearer(apiKey, { method: 'DELETE' }));
}

describe.skipIf(noPrivate.length > 0)(
  `registering a private repository${needed(noPrivate)}`,
  () => {
    it('copies it in and reaches ready, without the token appearing anywhere', async () => {
      const slug = uniqueSlug('private');
      try {
        const created = await call(
          '/api/v1/workspaces',
          asJson('POST', { slug, git_url: gitUrl, git_pat: gitToken }, apiKey),
        );
        const body = await created.text();

        expect(created.status).toBe(201);
        expect(JSON.parse(body)).toMatchObject({ status: 'pending' });
        expect(body).not.toContain(gitToken);

        const workspace = await settled(slug);
        expect(workspace.status).toBe('ready');
        expect(workspace.head_sha).toMatch(/^[0-9a-f]{40}$/);

        const listed = await call(`/api/v1/workspaces/${slug}/secrets`, bearer(apiKey));
        expect(await listed.text()).not.toContain(gitToken);
      } finally {
        await retire(slug);
      }
    }, 180_000);
  },
);

describe.skipIf(noRemote.length > 0)(`a copy that cannot finish${needed(noRemote)}`, () => {
  it('leaves a readable workspace that says why, and can still be archived', async () => {
    const slug = uniqueSlug('failed');
    try {
      const created = await call(
        '/api/v1/workspaces',
        asJson(
          'POST',
          // A host that resolves to nothing: the create is accepted and the
          // copy is what discovers the trouble.
          { slug, git_url: 'https://nothing.invalid/owner/repo.git' },
          apiKey,
        ),
      );
      expect(created.status).toBe(201);

      const workspace = await settled(slug);
      expect(workspace.status).toBe('failed');
      expect(workspace.clone_error).not.toBeNull();
      if (gitToken.length > 0) {
        expect(workspace.clone_error ?? '').not.toContain(gitToken);
      }

      const archived = await call(
        `/api/v1/workspaces/${slug}/archive`,
        bearer(apiKey, { method: 'POST' }),
      );
      expect(archived.status).toBe(200);
    } finally {
      await retire(slug);
    }
  }, 180_000);
});

describe.skipIf(noRemote.length > 0)(`retiring a workspace${needed(noRemote)}`, () => {
  it('archives it and then deletes it, leaving nothing behind', async () => {
    const slug = uniqueSlug('retire');
    const created = await call(
      '/api/v1/workspaces',
      asJson(
        'POST',
        { slug, git_url: gitUrl, ...(gitToken.length > 0 ? { git_pat: gitToken } : {}) },
        apiKey,
      ),
    );
    expect(created.status).toBe(201);
    await settled(slug);

    const archived = await call(
      `/api/v1/workspaces/${slug}/archive`,
      bearer(apiKey, { method: 'POST' }),
    );
    expect(archived.status).toBe(200);
    expect(((await archived.json()) as { status: string }).status).toBe('archived');

    const deleted = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey, { method: 'DELETE' }));
    expect(deleted.status).toBe(204);

    const gone = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey));
    expect(gone.status).toBe(404);
  }, 180_000);
});
