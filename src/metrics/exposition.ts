import type { MetricSeries } from '../platform/metrics.js';
import { DOCUMENTED_METRICS, type DocumentedMetric } from './registry.js';

export interface ExpositionSample {
  labels: Record<string, string>;
  value: number;
  observations?: number;
}

export interface ExpositionFamily {
  name: string;
  type: string;
  labelNames: string[];
  samples: ExpositionSample[];
  buckets?: number[];
}

function escapeLabelValue(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/\n/g, '\\n').replace(/"/g, '\\"');
}

function labelSet(metric: DocumentedMetric, labels: Readonly<Record<string, string>>): string {
  if (metric.labels.length === 0) {
    return '';
  }
  const pairs = metric.labels.map((name) => `${name}="${escapeLabelValue(labels[name] ?? '')}"`);
  return `{${pairs.join(',')}}`;
}

/**
 * Renders the recorded series as Prometheus text exposition. Every metric the
 * reference names is written, whether or not this service has a source for it
 * yet: one with no series is written once with the value 0, so a scraper's
 * queries do not start failing the day it gains one.
 */
export function renderExposition(series: readonly MetricSeries[]): string {
  const lines: string[] = [];

  for (const metric of DOCUMENTED_METRICS) {
    lines.push(`# HELP ${metric.name} ${metric.help}`);
    lines.push(`# TYPE ${metric.name} ${metric.type}`);
    if (metric.buckets !== undefined) {
      lines.push(`# BUCKETS ${metric.name} ${metric.buckets.join(',')}`);
    }

    const recorded = series.filter((s) => s.name === metric.name);
    if (recorded.length === 0) {
      lines.push(`${metric.name}${labelSet(metric, {})} 0`);
      continue;
    }
    for (const sample of recorded) {
      const suffix = sample.observations === undefined ? '' : ` ${sample.observations}`;
      lines.push(`${metric.name}${labelSet(metric, sample.labels)} ${sample.value}${suffix}`);
    }
  }

  return `${lines.join('\n')}\n`;
}

const SAMPLE = /^(?<name>[a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(?<labels>[^}]*)\})? (?<rest>.+)$/;

/** Reads back what {@link renderExposition} wrote, for the tests to assert on. */
export function parseExposition(text: string): Map<string, ExpositionFamily> {
  const families = new Map<string, ExpositionFamily>();

  for (const line of text.split('\n')) {
    if (line.length === 0) {
      continue;
    }

    if (line.startsWith('# TYPE ')) {
      const [, , name, type] = line.split(' ');
      if (name !== undefined && type !== undefined) {
        const documented = DOCUMENTED_METRICS.find((m) => m.name === name);
        families.set(name, {
          name,
          type,
          labelNames: [...(documented?.labels ?? [])],
          samples: [],
        });
      }
      continue;
    }

    if (line.startsWith('# BUCKETS ')) {
      const [, , name, buckets] = line.split(' ');
      const family = name === undefined ? undefined : families.get(name);
      if (family !== undefined && buckets !== undefined) {
        family.buckets = buckets.split(',').map(Number);
      }
      continue;
    }

    if (line.startsWith('#')) {
      continue;
    }

    const match = SAMPLE.exec(line);
    const name = match?.groups?.['name'];
    const family = name === undefined ? undefined : families.get(name);
    if (match === undefined || match === null || family === undefined) {
      continue;
    }

    const labels: Record<string, string> = {};
    for (const pair of (match.groups?.['labels'] ?? '').split(',')) {
      const eq = pair.indexOf('=');
      if (eq > 0) {
        labels[pair.slice(0, eq)] = pair.slice(eq + 1).replace(/^"|"$/g, '');
      }
    }

    const parts = (match.groups?.['rest'] ?? '').split(' ');
    const sample: ExpositionSample = { labels, value: Number(parts[0]) };
    if (parts[1] !== undefined) {
      sample.observations = Number(parts[1]);
    }
    family.samples.push(sample);
  }

  return families;
}
