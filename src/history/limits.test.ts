import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { ENGINE_LIMITS, type Measured, withinLimits } from './limits.js';

/**
 * Each of the four ceilings, exceeded in turn. The work must be refused
 * before it starts, and the refusal must say which ceiling it was.
 */
const CASES: Array<{ what: string; measured: Measured; names: string }> = [
  {
    what: 'the patch count',
    measured: { patches: ENGINE_LIMITS.patches + 1 },
    names: 'patches',
  },
  {
    what: 'the repository size',
    measured: { repositoryBytes: ENGINE_LIMITS.repositoryBytes + 1 },
    names: 'packed objects',
  },
  {
    what: 'the changed-file count',
    measured: { changedFiles: ENGINE_LIMITS.changedFiles + 1 },
    names: 'files',
  },
  {
    what: 'the diff size',
    measured: { diffBytes: ENGINE_LIMITS.diffBytes + 1 },
    names: 'diff',
  },
];

describe('the sizes an operation may work at', () => {
  it.each(CASES)('refuses $what and says so', ({ measured, names }) => {
    let thrown: unknown;
    try {
      withinLimits(measured);
    } catch (err) {
      thrown = err;
    }

    expect(thrown, 'the operation was not refused').toBeInstanceOf(HttpError);
    expect((thrown as HttpError).status).toBe(413);
    expect((thrown as HttpError).message).toContain(names);
  });

  it.each(CASES)('allows $what at the limit itself', ({ measured }) => {
    const [key, over] = Object.entries(measured)[0] as [keyof Measured, number];
    expect(() => {
      withinLimits({ [key]: over - 1 });
    }).not.toThrow();
  });

  it('allows an operation that stays under every limit', () => {
    expect(() => {
      withinLimits({ patches: 50, repositoryBytes: 1024, changedFiles: 3, diffBytes: 400 });
    }).not.toThrow();
  });

  it('says how much was asked for and how much is allowed', () => {
    expect(() => {
      withinLimits({ patches: 51 });
    }).toThrow(/51[\s\S]*50/);
  });
});
