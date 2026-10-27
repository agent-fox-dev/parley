import { describe, expect, it } from 'vitest';
import { DOCUMENTED_METRICS, HTTP_DURATION_BUCKETS } from './registry.js';
import { parseExposition } from './exposition.js';
import { buildApp } from '../http/router.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';

async function exposition(app: ReturnType<typeof buildApp>) {
  const response = await app.request('/metrics');
  return { response, families: parseExposition(await response.text()) };
}

describe('the metrics route', () => {
  it('answers in the Prometheus text exposition format', async () => {
    const { response } = await exposition(buildApp({ metrics: inMemoryMetrics() }));

    expect(response.status).toBe(200);
    expect(response.headers.get('content-type')).toBe('text/plain; version=0.0.4');
  });

  it.each(DOCUMENTED_METRICS.map((m) => [m.name, m] as const))(
    'exposes %s with its documented type and label names',
    async (_name, metric) => {
      const { families } = await exposition(buildApp({ metrics: inMemoryMetrics() }));

      const family = families.get(metric.name);
      expect(family, `${metric.name} is absent from the exposition`).toBeDefined();
      expect(family?.type).toBe(metric.type);
      expect(family?.labelNames.toSorted()).toEqual([...metric.labels].toSorted());
    },
  );

  it.each(DOCUMENTED_METRICS.filter((m) => m.labels.length === 0).map((m) => m.name))(
    'exposes %s with the value 0 on a service that has measured nothing',
    async (name) => {
      const { families } = await exposition(buildApp({ metrics: inMemoryMetrics() }));

      expect(families.get(name)?.samples[0]?.value).toBe(0);
    },
  );

  it('lists the documented histogram buckets for the request duration', async () => {
    const { families } = await exposition(buildApp({ metrics: inMemoryMetrics() }));

    expect(families.get('afhub_http_request_duration_seconds')?.buckets).toEqual(
      HTTP_DURATION_BUCKETS,
    );
  });
});

describe('the path label of an unmatched request', () => {
  it('takes one value however many distinct paths are requested', async () => {
    const app = buildApp({ metrics: inMemoryMetrics() });

    await Promise.all(Array.from({ length: 50 }, (_unused, i) => app.request(`/api/v1/nope-${i}`)));
    const { families } = await exposition(app);

    const notFound = (families.get('afhub_http_requests_total')?.samples ?? []).filter(
      (s) => s.labels.status === '404',
    );
    const paths = new Set(notFound.map((s) => s.labels.path));

    expect(paths.size).toBe(1);
    expect([...paths][0]).not.toMatch(/nope-\d+/);
    expect(notFound[0]?.value).toBe(50);
  });
});
