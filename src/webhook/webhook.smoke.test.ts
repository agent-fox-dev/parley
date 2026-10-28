import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, it } from 'vitest';
import { afc, apiKey, asJson, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * The webhook end to end against a running deployment: a secret set
 * through the API, a delivery signed as GitHub signs one, and the
 * event reaching the audit query and the log. The forge itself is
 * not involved — a delivery is a POST, and that is the whole point
 * of the route.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
);

/** The signature GitHub would send for this body and this secret. */
async function signatureOf(body: string, secret: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const mac = new Uint8Array(
    await crypto.subtle.sign(
      'HMAC',
      key,
      new TextEncoder().encode(body) as unknown as BufferSource,
    ),
  );
  return `sha256=${[...mac].map((byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

/** The repository the deployment's workspace really points at. */
async function repositoryOf(): Promise<string> {
  const described = (await (
    await call(`/api/v1/workspaces/${slug}/forge`, bearer(apiKey))
  ).json()) as { repository: { owner: string; name: string } | null };
  const repository = described.repository;
  return repository === null ? 'acme/widgets' : `${repository.owner}/${repository.name}`;
}

async function afcRun(...args: string[]): Promise<{ code: number; stdout: string }> {
  return run(afc, args, {
    env: { ...process.env, ENDPOINT_URL: baseUrl, API_KEY: apiKey },
    timeout: 120_000,
  })
    .then((done) => ({ code: 0, stdout: done.stdout }))
    .catch((err: { code?: number; stdout?: string }) => ({
      code: err.code ?? 1,
      stdout: err.stdout ?? '',
    }));
}

describe.skipIf(reason.length > 0)(`the webhook end to end${needed(reason)}`, () => {
  it('is set up, takes a signed delivery, and relays it where it can be read', async () => {
    const set = await call(`/api/v1/workspaces/${slug}/forge/webhook`, asJson('PUT', {}, apiKey));
    expect(set.status).toBe(200);
    const { secret, url } = (await set.json()) as { secret: string; url: string | null };
    expect(secret).toMatch(/^[0-9a-f]{64}$/);

    try {
      const id = `smoke-${Date.now().toString(36)}`;
      const payload = JSON.stringify({
        action: 'opened',
        repository: { full_name: await repositoryOf() },
        issue: { number: 1, title: 'a smoke delivery', html_url: 'https://example.test/i/1' },
        sender: { login: 'smoke' },
      });

      const delivered = await call(`/hooks/forge/${slug}`, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-hub-signature-256': await signatureOf(payload, secret),
          'x-github-delivery': id,
          'x-github-event': 'issues',
        },
        body: payload,
      });
      expect(delivered.status).toBe(202);
      expect((await delivered.json()) as { outcome: string }).toEqual({ outcome: 'relayed' });

      // It reaches the log…
      const logged = (await (
        await call(`/api/v1/workspaces/${slug}/forge/deliveries?limit=100`, bearer(apiKey))
      ).json()) as Array<{ delivery_id: string; outcome: string; relayed: string[] }>;
      const row = logged.find((one) => one.delivery_id === id);
      expect(row?.outcome).toBe('relayed');
      expect(row?.relayed).toEqual(['hub.forge.issue.opened']);

      // …and the audit query, through the recorder.
      for (let look = 0; look < 20; look += 1) {
        const found = (await (
          await call(
            `/api/v1/audit?workspace=${slug}&event_type=hub.forge.issue.opened&limit=5`,
            bearer(apiKey),
          )
        ).json()) as { events: Array<{ id: string }> };
        if (found.events.length > 0) {
          expect(url === null ? true : url.endsWith(`/hooks/forge/${slug}`)).toBe(true);
          return;
        }
        await new Promise((settled) => setTimeout(settled, 500));
      }
      expect(url === null ? true : url.endsWith(`/hooks/forge/${slug}`)).toBe(true);
      expect.fail('the relayed event never reached the audit query');
    } finally {
      await call(`/api/v1/workspaces/${slug}/forge/webhook`, bearer(apiKey, { method: 'DELETE' }));
    }
  }, 180_000);

  it('answers a redelivery as a duplicate and adds nothing', async () => {
    const set = await call(`/api/v1/workspaces/${slug}/forge/webhook`, asJson('PUT', {}, apiKey));
    const { secret } = (await set.json()) as { secret: string };

    try {
      const id = `smoke-twice-${Date.now().toString(36)}`;
      const payload = JSON.stringify({
        action: 'opened',
        repository: { full_name: await repositoryOf() },
        issue: { number: 1 },
      });
      const headers = {
        'content-type': 'application/json',
        'x-hub-signature-256': await signatureOf(payload, secret),
        'x-github-delivery': id,
        'x-github-event': 'issues',
      };

      const first = await call(`/hooks/forge/${slug}`, { method: 'POST', headers, body: payload });
      const again = await call(`/hooks/forge/${slug}`, { method: 'POST', headers, body: payload });

      expect(first.status).toBe(202);
      expect((await again.json()) as { outcome: string }).toEqual({ outcome: 'duplicate' });

      const logged = (await (
        await call(`/api/v1/workspaces/${slug}/forge/deliveries?limit=100`, bearer(apiKey))
      ).json()) as Array<{ delivery_id: string }>;
      expect(logged.filter((one) => one.delivery_id === id)).toHaveLength(1);
    } finally {
      await call(`/api/v1/workspaces/${slug}/forge/webhook`, bearer(apiKey, { method: 'DELETE' }));
    }
  }, 180_000);

  it('refuses an unsigned and a wrongly signed delivery, recording neither', async () => {
    const set = await call(`/api/v1/workspaces/${slug}/forge/webhook`, asJson('PUT', {}, apiKey));
    expect(set.status).toBe(200);

    try {
      const payload = JSON.stringify({ action: 'opened', repository: { full_name: 'a/b' } });
      const unsigned = await call(`/hooks/forge/${slug}`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'x-github-event': 'issues' },
        body: payload,
      });
      const wrong = await call(`/hooks/forge/${slug}`, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-hub-signature-256': `sha256=${'a'.repeat(64)}`,
          'x-github-delivery': 'smoke-wrong',
          'x-github-event': 'issues',
        },
        body: payload,
      });

      expect([unsigned.status, wrong.status]).toEqual([401, 401]);
      const logged = (await (
        await call(`/api/v1/workspaces/${slug}/forge/deliveries?limit=100`, bearer(apiKey))
      ).json()) as Array<{ delivery_id: string }>;
      expect(logged.map((one) => one.delivery_id)).not.toContain('smoke-wrong');
    } finally {
      await call(`/api/v1/workspaces/${slug}/forge/webhook`, bearer(apiKey, { method: 'DELETE' }));
    }
  }, 180_000);

  it.skipIf(afc.length === 0)(
    'answers afc forge webhook set, unset and deliveries',
    async () => {
      const set = await afcRun('forge', 'webhook', 'set', slug);
      const listed = await afcRun('forge', 'deliveries', slug);
      const unset = await afcRun('forge', 'webhook', 'unset', slug);

      expect(set.code).toBe(0);
      expect(listed.code).toBe(0);
      expect(unset.code).toBe(0);
    },
    180_000,
  );
});
