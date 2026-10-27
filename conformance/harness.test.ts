import { describe, expect, it } from 'vitest';
import { type Check, runHarness } from './harness.js';

const passing: Check = { name: 'ok', run: async () => ({ outcome: 'pass' }) };
const failing: Check = {
  name: 'bad',
  run: async () => ({ outcome: 'fail', message: 'answered 500' }),
};
const skipping: Check = {
  name: 'skipped',
  needs: () => 'a deployment',
  run: async () => ({ outcome: 'pass' }),
};

describe('the harness report and exit code', () => {
  it('names every check with one of the three outcomes', async () => {
    const result = await runHarness({
      baseUrl: 'http://stub.invalid',
      checks: [passing, failing, skipping],
    });

    expect(Object.fromEntries(result.report.map((c) => [c.name, c.outcome]))).toEqual({
      ok: 'pass',
      bad: 'fail',
      skipped: 'skip',
    });
  });

  it('exits non-zero when a check failed', async () => {
    const result = await runHarness({
      baseUrl: 'http://stub.invalid',
      checks: [passing, failing, skipping],
    });

    expect(result.exitCode).not.toBe(0);
  });

  it('exits 0 when the only checks that did not pass were skipped', async () => {
    const result = await runHarness({
      baseUrl: 'http://stub.invalid',
      checks: [passing, skipping],
    });

    expect(result.exitCode).toBe(0);
  });

  it('exits 0 for a run with no checks at all', async () => {
    const result = await runHarness({ baseUrl: 'http://stub.invalid', checks: [] });

    expect(result.exitCode).toBe(0);
    expect(result.report).toEqual([]);
  });
});

describe('a check whose dependency is missing', () => {
  it.each([
    ['a binary', 'the afc binary'],
    ['a network', 'a network'],
    ['a deployment', 'a deployment'],
  ])('is skipped with a reason naming %s', async (_kind, dependency) => {
    const dependent: Check = {
      name: 'dependent',
      needs: () => dependency,
      run: async () => ({ outcome: 'pass' }),
    };

    const withDependent = await runHarness({
      baseUrl: 'http://stub.invalid',
      checks: [passing, dependent],
    });
    const withoutDependent = await runHarness({
      baseUrl: 'http://stub.invalid',
      checks: [passing],
    });

    const entry = withDependent.report.find((c) => c.name === 'dependent');
    expect(entry?.outcome).toBe('skip');
    expect(entry?.message).toContain(dependency);
    expect(withDependent.exitCode).toBe(withoutDependent.exitCode);
  });

  it('runs a check whose dependencies are all present', async () => {
    const satisfied: Check = {
      name: 'satisfied',
      needs: () => undefined,
      run: async () => ({ outcome: 'pass' }),
    };

    const result = await runHarness({ baseUrl: 'http://stub.invalid', checks: [satisfied] });

    expect(result.report[0]?.outcome).toBe('pass');
  });
});
