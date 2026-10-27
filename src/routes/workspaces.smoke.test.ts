import { describe, expect, it } from 'vitest';

/**
 * Registration, membership, confinement and retirement against a
 * deployment that is really running. Each suite names what it needs and
 * skips saying so rather than passing on nothing.
 */
const baseUrl = (process.env['PARLEY_BASE_URL'] ?? '').replace(/\/$/, '');
const apiKey = process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const memberKey = process.env['PARLEY_SMOKE_MEMBER_KEY'] ?? '';

const missing = (...needs: Array<[string, string]>): string =>
  needs
    .filter(([, value]) => value.length === 0)
    .map(([name]) => name)
    .join(', ');

const noOwner = missing(['PARLEY_BASE_URL', baseUrl], ['PARLEY_SMOKE_API_KEY', apiKey]);
const noMember = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_MEMBER_KEY', memberKey],
);

const bearer = (credential: string, init: RequestInit = {}): RequestInit => ({
  ...init,
  headers: {
    ...((init.headers ?? {}) as Record<string, string>),
    authorization: `Bearer ${credential}`,
  },
});

const asJson = (method: string, body: unknown, credential: string): RequestInit =>
  bearer(credential, {
    method,
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });

async function call(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(`${baseUrl}${path}`, init);
}

/** A slug of this run alone, so repeated runs do not collide. */
const slugOf = (what: string) => `smoke-${what}-${Date.now().toString(36)}`;

async function register(slug: string, credential: string): Promise<Response> {
  return call(
    '/api/v1/workspaces',
    asJson(
      'POST',
      {
        slug,
        git_url: 'https://github.com/agent-fox-dev/parley.git',
        git_pat: 'ghp_SMOKE_SENTINEL',
      },
      credential,
    ),
  );
}

async function retire(slug: string, credential: string): Promise<void> {
  await call(`/api/v1/workspaces/${slug}/archive`, bearer(credential, { method: 'POST' }));
  await call(`/api/v1/workspaces/${slug}`, bearer(credential, { method: 'DELETE' }));
}

describe.skipIf(noOwner.length > 0)(
  `registering a workspace${noOwner.length > 0 ? ` (needs ${noOwner})` : ''}`,
  () => {
    it('reads back what was written, and never the credential that was stored', async () => {
      const slug = slugOf('register');
      try {
        const created = await register(slug, apiKey);
        const body = (await created.json()) as Record<string, unknown>;

        expect(created.status).toBe(201);
        expect(body['status']).toBe('pending');
        expect(body['access']).toBe('owner');
        expect(JSON.stringify(body)).not.toContain('SMOKE_SENTINEL');

        const read = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey));
        const readBody = (await read.json()) as { settings: Record<string, unknown> };
        expect(read.status).toBe(200);
        expect(Object.keys(readBody.settings)).toHaveLength(12);

        const listed = (await (await call('/api/v1/workspaces', bearer(apiKey))).json()) as Array<{
          slug: string;
        }>;
        expect(listed.map((w) => w.slug)).toContain(slug);
      } finally {
        await retire(slug, apiKey);
      }
    });
  },
);

describe.skipIf(noMember.length > 0)(
  `a member of the organization${noMember.length > 0 ? ` (needs ${noMember})` : ''}`,
  () => {
    it('reads the workspace, cannot change it and cannot see its bound tokens', async () => {
      const slug = slugOf('member');
      try {
        expect((await register(slug, apiKey)).status).toBe(201);

        const read = await call(`/api/v1/workspaces/${slug}`, bearer(memberKey));
        expect(read.status).toBe(200);
        expect(((await read.json()) as { access: string }).access).toBe('member');

        const update = await call(
          `/api/v1/workspaces/${slug}`,
          asJson('PATCH', { display_name: 'Renamed by a member' }, memberKey),
        );
        const body = (await update.json()) as { error: { error_type?: string } };
        expect(update.status).toBe(403);
        expect(body.error.error_type).toBe('forbidden');

        const tokens = await call(`/api/v1/workspaces/${slug}/tokens`, bearer(memberKey));
        expect(tokens.status).toBe(404);
        expect(((await tokens.json()) as { error: { message: string } }).error.message).toBe(
          'workspace not found',
        );
      } finally {
        await retire(slug, apiKey);
      }
    });
  },
);

describe.skipIf(noOwner.length > 0)(
  `a token bound to one workspace${noOwner.length > 0 ? ` (needs ${noOwner})` : ''}`,
  () => {
    it('reaches that workspace and nothing else', async () => {
      const mine = slugOf('bound-mine');
      const other = slugOf('bound-other');
      try {
        expect((await register(mine, apiKey)).status).toBe(201);
        expect((await register(other, apiKey)).status).toBe(201);

        const minted = await call(
          `/api/v1/workspaces/${mine}/tokens`,
          asJson('POST', { name: 'smoke bound', permissions: ['workspaces:read'] }, apiKey),
        );
        expect(minted.status).toBe(201);
        const token = ((await minted.json()) as { token: string }).token;

        expect((await call(`/api/v1/workspaces/${mine}`, bearer(token))).status).toBe(200);

        const elsewhere = await call(`/api/v1/workspaces/${other}`, bearer(token));
        expect(elsewhere.status).toBe(404);
        expect(((await elsewhere.json()) as { error: { message: string } }).error.message).toBe(
          'workspace not found',
        );

        const creating = await call(
          '/api/v1/workspaces',
          asJson('POST', { slug: slugOf('never'), git_url: 'https://example.test/r.git' }, token),
        );
        const body = (await creating.json()) as { error: { message: string; error_type?: string } };
        expect(creating.status).toBe(403);
        expect(body.error.error_type).toBe('token_bound');
        expect(body.error.message).toContain(mine);
      } finally {
        await retire(mine, apiKey);
        await retire(other, apiKey);
      }
    });
  },
);

describe.skipIf(noOwner.length > 0)(
  `retiring a workspace${noOwner.length > 0 ? ` (needs ${noOwner})` : ''}`,
  () => {
    it('archives it, refuses a write, deletes it and kills its bound token', async () => {
      const slug = slugOf('retire');
      expect((await register(slug, apiKey)).status).toBe(201);
      const token = (
        (await (
          await call(
            `/api/v1/workspaces/${slug}/tokens`,
            asJson('POST', { name: 'smoke bound', permissions: ['workspaces:read'] }, apiKey),
          )
        ).json()) as { token: string }
      ).token;

      const archived = await call(
        `/api/v1/workspaces/${slug}/archive`,
        bearer(apiKey, { method: 'POST' }),
      );
      expect(archived.status).toBe(200);
      expect(((await archived.json()) as { status: string }).status).toBe('archived');

      const update = await call(
        `/api/v1/workspaces/${slug}`,
        asJson('PATCH', { display_name: 'too late' }, apiKey),
      );
      expect(update.status).toBe(400);

      const deleted = await call(
        `/api/v1/workspaces/${slug}`,
        bearer(apiKey, { method: 'DELETE' }),
      );
      expect(deleted.status).toBe(204);

      const listed = (await (
        await call('/api/v1/workspaces?include_archived=true', bearer(apiKey))
      ).json()) as Array<{ slug: string }>;
      expect(listed.map((w) => w.slug)).not.toContain(slug);

      expect((await call('/api/v1/user', bearer(token))).status).toBe(401);
    });
  },
);
