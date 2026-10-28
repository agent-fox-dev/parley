export interface CounterHandle {
  /** Adds one to the series carrying these label values. */
  increment(labels: Readonly<Record<string, string>>): void;
}

export interface GaugeHandle {
  /** Sets the series carrying these label values to this value. */
  set(labels: Readonly<Record<string, string>>, value: number): void;
}

export interface HistogramHandle {
  /** Records one observation against the series carrying these label values. */
  observe(labels: Readonly<Record<string, string>>, value: number): void;
}

/** Where the service records what it measured. */
export interface MetricsSink {
  counter(name: string): CounterHandle;
  gauge(name: string): GaugeHandle;
  histogram(name: string): HistogramHandle;
  /** Every series recorded so far, for the exposition to render. */
  snapshot(): MetricSeries[];
}

export interface MetricSeries {
  name: string;
  labels: Readonly<Record<string, string>>;
  value: number;
  observations?: number;
}
