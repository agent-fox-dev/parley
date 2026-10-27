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

const SENTINEL = 'SMOKE-secret-value-9f3a';
const absent = missing(['PARLEY_BASE_URL', baseUrl], ['PARLEY_SMOKE_API_KEY', apiKey]);

async function register(slug: string): Promise<void> {
  const created = await call(
    '/api/v1/workspaces',
    asJson('POST', { slug, git_url: 'https://github.com/agent-fox-dev/parley.git' }, apiKey),
  );
  expect(created.status).toBe(201);
}

async function retire(slug: string): Promise<void> {
  await call(`/api/v1/workspaces/${slug}/archive`, bearer(apiKey, { method: 'POST' }));
  await call(`/api/v1/workspaces/${slug}`, bearer(apiKey, { method: 'DELETE' }));
}

describe.skipIf(absent.length > 0)(`a workspace secret${needed(absent)}`, () => {
  it('is stored, listed by key alone, and never comes back out', async () => {
    const slug = uniqueSlug('secret');
    try {
      await register(slug);
      const path = `/api/v1/workspaces/${slug}/secrets`;

      const created = await call(
        path,
        asJson('POST', { entries: [{ key: 'SMOKE_SECRET', value: SENTINEL }] }, apiKey),
      );
      const createdText = await created.text();

      expect(created.status).toBe(201);
      expect(createdText).toContain('SMOKE_SECRET');
      expect(createdText).not.toContain(SENTINEL);

      const listed = await call(path, bearer(apiKey));
      const listedText = await listed.text();

      expect(listed.status).toBe(200);
      expect(listedText).toContain('SMOKE_SECRET');
      expect(listedText).not.toContain(SENTINEL);
      expect(listedText).not.toContain('"value"');

      const replaced = await call(
        `${path}/SMOKE_SECRET`,
        asJson('PATCH', { value: 'a replacement' }, apiKey),
      );
      expect(replaced.status).toBe(200);
      expect(await replaced.text()).not.toContain('a replacement');

      expect(
        (await call(`${path}/SMOKE_SECRET`, bearer(apiKey, { method: 'DELETE' }))).status,
      ).toBe(204);
    } finally {
      await retire(slug);
    }
  });
});
