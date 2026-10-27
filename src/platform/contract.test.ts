import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { d1Store } from './cloudflare/store.js';
import { envConfig } from './cloudflare/config.js';
import { fixedClock } from './local/clock.js';
import { inMemoryMetrics } from './local/metrics.js';
import { processConfig } from './local/config.js';
import { sqliteStore } from './local/store.js';
import { systemClock } from './local/clock.js';
import type { Clock, ConfigSource, MetricsSink, RelationalStore } from './index.js';

/**
 * One suite per interface, run against every implementation the deployment
 * can be wired with. A case that passes against one and fails against
 * another is a defect in the interface, so the suites below are the same
 * code for every implementation, parameterised only by what they are given.
 */

interface Candidate<T> {
  name: string;
  make: () => T | Promise<T>;
  /** Puts the subject into the state the suite needs to see a failure. */
  breakIt?: (subject: T) => void;
}

function stubD1(answer: () => unknown) {
  return {
    prepare: () => ({
      first: async () => answer(),
    }),
  };
}

describe.each<Candidate<RelationalStore>>([
  {
    name: 'the Cloudflare store over D1',
    make: () => d1Store(stubD1(() => ({ one: 1 })) as never),
  },
  { name: 'the local store over SQLite', make: () => sqliteStore(':memory:') },
])('the relational store contract: $name', ({ make }) => {
  it('resolves when the store answers', async () => {
    await expect((await make()).ping()).resolves.toBeUndefined();
  });

  it('can be pinged repeatedly', async () => {
    const store = await make();

    await store.ping();
    await expect(store.ping()).resolves.toBeUndefined();
  });
});

/**
 * D1 over SQLite, so the Cloudflare adapter's own binding and batching code
 * is exercised rather than a stub that agrees with it.
 */
function fakeD1(path: string) {
  const db = new DatabaseSync(path);
  const statement = (sql: string, params: unknown[] = []) => ({
    bind: (...bound: unknown[]) => statement(sql, bound),
    first: async () => Promise.resolve(db.prepare(sql).get(...(params as never[])) ?? null),
    all: async () => Promise.resolve({ results: db.prepare(sql).all(...(params as never[])) }),
    run: async () => Promise.resolve(db.prepare(sql).run(...(params as never[]))),
    sql,
    params,
  });
  return {
    prepare: (sql: string) => statement(sql),
    batch: async (statements: Array<{ sql: string; params: unknown[] }>) => {
      db.exec('BEGIN');
      try {
        for (const one of statements) {
          db.prepare(one.sql).run(...(one.params as never[]));
        }
        db.exec('COMMIT');
      } catch (err) {
        db.exec('ROLLBACK');
        throw err;
      }
      return Promise.resolve([]);
    },
  };
}

const storeDirs: string[] = [];

function tempPath(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-contract-'));
  storeDirs.push(dir);
  return join(dir, 'parley.db');
}

afterEach(() => {
  for (const dir of storeDirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

describe.each<Candidate<RelationalStore>>([
  {
    name: 'the Cloudflare store over D1',
    make: () => d1Store(fakeD1(tempPath()) as never),
  },
  { name: 'the local store over SQLite', make: () => sqliteStore(tempPath()) },
])('the relational store contract: reading and writing with $name', ({ make }) => {
  async function withTable(): Promise<RelationalStore> {
    const store = await make();
    await store.run('CREATE TABLE things (id TEXT PRIMARY KEY, label TEXT NOT NULL)');
    return store;
  }

  it('reads back the rows it wrote, with the values bound in order', async () => {
    const store = await withTable();

    await store.run('INSERT INTO things (id, label) VALUES (?, ?)', ['a', 'first']);
    await store.run('INSERT INTO things (id, label) VALUES (?, ?)', ['b', 'second']);

    expect(await store.all('SELECT label FROM things WHERE id = ?', ['b'])).toEqual([
      { label: 'second' },
    ]);
  });

  it('answers a select that matches nothing with no rows', async () => {
    const store = await withTable();

    expect(await store.all('SELECT * FROM things WHERE id = ?', ['missing'])).toEqual([]);
  });

  it('applies every statement of a batch', async () => {
    const store = await withTable();

    await store.batch([
      { sql: 'INSERT INTO things (id, label) VALUES (?, ?)', params: ['a', 'first'] },
      { sql: 'INSERT INTO things (id, label) VALUES (?, ?)', params: ['b', 'second'] },
    ]);

    expect(await store.all('SELECT id FROM things ORDER BY id')).toEqual([
      { id: 'a' },
      { id: 'b' },
    ]);
  });

  it('leaves nothing behind when one statement of a batch fails', async () => {
    const store = await withTable();

    await expect(
      store.batch([
        { sql: 'INSERT INTO things (id, label) VALUES (?, ?)', params: ['a', 'first'] },
        { sql: 'INSERT INTO things (id, label) VALUES (?, ?)', params: ['a', 'again'] },
      ]),
    ).rejects.toThrow();

    expect(await store.all('SELECT id FROM things')).toEqual([]);
  });

  it('rejects a statement the store cannot run', async () => {
    const store = await withTable();

    await expect(store.all('SELECT * FROM absent')).rejects.toThrow();
  });
});

describe('the relational store contract: a store that does not answer', () => {
  it.each([
    [
      'the Cloudflare store over D1',
      () =>
        d1Store(
          stubD1(() => {
            throw new Error('D1_ERROR: no such table');
          }) as never,
        ),
    ],
    ['the local store over SQLite', () => sqliteStore('/nonexistent-directory/parley.db')],
  ])('rejects for %s', async (_name, make) => {
    await expect(Promise.resolve(make()).then((s) => s.ping())).rejects.toThrow();
  });
});

describe.each<Candidate<ConfigSource>>([
  { name: 'the Cloudflare config over the worker env', make: () => envConfig({ A: 'set', B: '' }) },
  {
    name: 'the local config over the process environment',
    make: () => processConfig({ A: 'set', B: '' }),
  },
])('the configuration source contract: $name', ({ make }) => {
  it('returns a value that is set', async () => {
    expect((await make()).get('A')).toBe('set');
  });

  it.each([
    ['a key that is not set', 'MISSING'],
    ['a key set to the empty string', 'B'],
  ])('returns undefined for %s', async (_name, key) => {
    expect((await make()).get(key)).toBeUndefined();
  });
});

describe.each<Candidate<Clock>>([
  { name: 'the system clock', make: () => systemClock() },
  {
    name: 'the fixed clock the tests use',
    make: () => fixedClock(new Date('2026-10-27T00:00:00Z')),
  },
])('the clock contract: $name', ({ make }) => {
  it('answers with a valid instant', async () => {
    const instant = (await make()).now();

    expect(instant).toBeInstanceOf(Date);
    expect(Number.isNaN(instant.getTime())).toBe(false);
  });

  it('never goes backwards', async () => {
    const clock = await make();

    const first = clock.now().getTime();
    const second = clock.now().getTime();

    expect(second).toBeGreaterThanOrEqual(first);
  });
});

describe.each<Candidate<MetricsSink>>([
  { name: 'the in-memory metrics sink', make: () => inMemoryMetrics() },
])('the metrics sink contract: $name', ({ make }) => {
  it('counts a series per label set', async () => {
    const sink = await make();
    const counter = sink.counter('requests_total');

    counter.increment({ method: 'GET', status: '200' });
    counter.increment({ method: 'GET', status: '200' });
    counter.increment({ method: 'GET', status: '404' });

    const series = sink.snapshot().filter((s) => s.name === 'requests_total');
    expect(series).toHaveLength(2);
    expect(series.find((s) => s.labels.status === '200')?.value).toBe(2);
    expect(series.find((s) => s.labels.status === '404')?.value).toBe(1);
  });

  it('records observations and their count on a histogram', async () => {
    const sink = await make();
    const histogram = sink.histogram('duration_seconds');

    histogram.observe({ method: 'GET' }, 0.5);
    histogram.observe({ method: 'GET' }, 1.5);

    const series = sink.snapshot().find((s) => s.name === 'duration_seconds');
    expect(series?.observations).toBe(2);
    expect(series?.value).toBeCloseTo(2);
  });

  it('starts with nothing recorded', async () => {
    expect((await make()).snapshot()).toEqual([]);
  });

  it('returns the same series for the same name and labels', async () => {
    const sink = await make();

    sink.counter('a_total').increment({ x: '1' });
    sink.counter('a_total').increment({ x: '1' });

    expect(sink.snapshot().filter((s) => s.name === 'a_total')).toHaveLength(1);
  });
});
