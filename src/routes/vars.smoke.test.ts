import { describe, expect, it } from 'vitest';
import {
  apiKey,
  asJson,
  baseUrl,
  bearer,
  call,
  memberKey,
  missing,
  needed,
  uniqueSlug,
} from '../testutil/smoke.js';

interface Resolved {
  key: string;
  value: string;
  origin: string;
}

const noOwner = missing(['PARLEY_BASE_URL', baseUrl], ['PARLEY_SMOKE_API_KEY', apiKey]);
const noMember = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_MEMBER_KEY', memberKey],
);

async function register(slug: string): Promise<void> {
  expect(
    (
      await call(
        '/api/v1/workspaces',
        asJson('POST', { slug, git_url: 'https://github.com/agent-fox-dev/parley.git' }, apiKey),
      )
    ).status,
  ).toBe(201);
}

async function retire(slug: string, keys: string[]): Promise<void> {
  for (const key of keys) {
    await call(`/api/v1/user/vars/${key}`, bearer(apiKey, { method: 'DELETE' }));
  }
  await call(`/api/v1/workspaces/${slug}/archive`, bearer(apiKey, { method: 'POST' }));
  await call(`/api/v1/workspaces/${slug}`, bearer(apiKey, { method: 'DELETE' }));
}

describe.skipIf(noOwner.length > 0)(
  `the resolved variables of a workspace${needed(noOwner)}`,
  () => {
    it('take the workspace value where there is one and the owner value otherwise', async () => {
      const slug = uniqueSlug('vars');
      const overridden = `SMOKE_OVERRIDDEN_${Date.now().toString(36).toUpperCase()}`;
      const userOnly = `smoke_user_only_${Date.now().toString(36)}`;
      try {
        await register(slug);

        expect(
          (
            await call(
              '/api/v1/user/vars',
              asJson(
                'POST',
                {
                  entries: [
                    { key: overridden, value: 'from the user' },
                    { key: userOnly, value: 'user only' },
                  ],
                },
                apiKey,
              ),
            )
          ).status,
        ).toBe(201);

        expect(
          (
            await call(
              `/api/v1/workspaces/${slug}/vars`,
              asJson(
                'POST',
                { entries: [{ key: overridden, value: 'from the workspace' }] },
                apiKey,
              ),
            )
          ).status,
        ).toBe(201);

        const response = await call(`/api/v1/workspaces/${slug}/vars/resolved`, bearer(apiKey));
        const resolved = (await response.json()) as Resolved[];
        const mine = resolved.filter((entry) => entry.key === overridden || entry.key === userOnly);

        expect(response.status).toBe(200);
        expect(mine.find((e) => e.key === overridden)).toMatchObject({
          value: 'from the workspace',
          origin: 'workspace',
        });
        expect(mine.find((e) => e.key === userOnly)).toMatchObject({ origin: 'user' });

        const order = resolved.map((entry) => entry.key.toLowerCase());
        expect(order).toEqual(order.toSorted());
      } finally {
        await retire(slug, [overridden, userOnly]);
      }
    });
  },
);

describe.skipIf(noMember.length > 0)(`a member of the organization${needed(noMember)}`, () => {
  it("reads a workspace's variables and is refused its writes and its secrets", async () => {
    const slug = uniqueSlug('member-vars');
    try {
      await register(slug);
      await call(
        `/api/v1/workspaces/${slug}/vars`,
        asJson('POST', { entries: [{ key: 'SMOKE_SHARED', value: 'a shared value' }] }, apiKey),
      );

      const listed = await call(`/api/v1/workspaces/${slug}/vars`, bearer(memberKey));
      const listedBody = (await listed.json()) as Array<{ key: string; value: string }>;
      expect(listed.status).toBe(200);
      expect(listedBody.find((e) => e.key === 'SMOKE_SHARED')?.value).toBe('a shared value');

      const refused = await call(
        `/api/v1/workspaces/${slug}/vars`,
        asJson('POST', { entries: [{ key: 'SMOKE_THEIRS', value: 'v' }] }, memberKey),
      );
      const error = (await refused.json()) as { error: { error_type?: string } };
      expect(refused.status).toBe(403);
      expect(error.error.error_type).toBe('forbidden');

      expect((await call(`/api/v1/workspaces/${slug}/secrets`, bearer(memberKey))).status).toBe(
        404,
      );
    } finally {
      await retire(slug, []);
    }
  });
});
