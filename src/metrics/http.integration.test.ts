import { describe, expect, it } from 'vitest';
import { HTTP_DURATION_BUCKETS } from './registry.js';
import { parseExposition } from './exposition.js';
import { buildApp } from '../http/router.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';

/**
 * The HTTP metrics against the service's real sink, with a real request
 * answered end to end before they are read.
 */
describe('an answered request', () => {
  it('is counted and timed under its method, route template and status', async () => {
    const app = buildApp({ metrics: inMemoryMetrics(), store: { ping: async () => {} } });

    await app.request('/healthz');
    const families = parseExposition(await (await app.request('/metrics')).text());

    const counted = families
      .get('afhub_http_requests_total')
      ?.samples.find(
        (s) =>
          s.labels.method === 'GET' && s.labels.path === '/healthz' && s.labels.status === '200',
      );
    expect(counted?.value).toBe(1);

    const timed = families
      .get('afhub_http_request_duration_seconds')
      ?.samples.find((s) => s.labels.method === 'GET' && s.labels.path === '/healthz');
    expect(timed?.observations).toBe(1);
  });

  it('is timed in the documented bucket boundaries', async () => {
    const app = buildApp({ metrics: inMemoryMetrics() });

    await app.request('/healthz');
    const families = parseExposition(await (await app.request('/metrics')).text());

    expect(families.get('afhub_http_request_duration_seconds')?.buckets).toEqual(
      HTTP_DURATION_BUCKETS,
    );
  });

  it('counts each status separately', async () => {
    const app = buildApp({ metrics: inMemoryMetrics() });

    await app.request('/healthz');
    await app.request('/healthz', { method: 'POST' });
    const families = parseExposition(await (await app.request('/metrics')).text());

    const samples = families.get('afhub_http_requests_total')?.samples ?? [];
    expect(
      samples.find((s) => s.labels.status === '200' && s.labels.path === '/healthz')?.value,
    ).toBe(1);
    expect(
      samples.find((s) => s.labels.status === '405' && s.labels.path === '/healthz')?.value,
    ).toBe(1);
  });
});
