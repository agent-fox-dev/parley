import type {
  CounterHandle,
  GaugeHandle,
  HistogramHandle,
  MetricSeries,
  MetricsSink,
} from '../metrics.js';

interface Cell {
  name: string;
  labels: Readonly<Record<string, string>>;
  value: number;
  observations?: number;
}

function key(name: string, labels: Readonly<Record<string, string>>): string {
  const parts = Object.keys(labels)
    .toSorted()
    .map((label) => `${label}=${labels[label] ?? ''}`);
  return `${name}{${parts.join(',')}}`;
}

/**
 * Metrics held in memory, which is where the exposition reads them from on
 * both targets. Nothing about counting differs by platform.
 */
export function inMemoryMetrics(): MetricsSink {
  const cells = new Map<string, Cell>();

  function cell(name: string, labels: Readonly<Record<string, string>>): Cell {
    const id = key(name, labels);
    let found = cells.get(id);
    if (found === undefined) {
      found = { name, labels: { ...labels }, value: 0 };
      cells.set(id, found);
    }
    return found;
  }

  return {
    counter(name: string): CounterHandle {
      return {
        increment(labels) {
          cell(name, labels).value += 1;
        },
      };
    },
    gauge(name: string): GaugeHandle {
      return {
        set(labels, value) {
          cell(name, labels).value = value;
        },
      };
    },
    histogram(name: string): HistogramHandle {
      return {
        observe(labels, value) {
          const target = cell(name, labels);
          target.value += value;
          target.observations = (target.observations ?? 0) + 1;
        },
      };
    },
    snapshot(): MetricSeries[] {
      return [...cells.values()].map((c) => structuredClone(c));
    },
  };
}
