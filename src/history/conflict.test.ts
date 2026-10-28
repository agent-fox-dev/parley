import { describe, expect, it } from 'vitest';
import { type Conflict, conflictIdentity } from './conflict.js';

const conflict = (over: Partial<Conflict> = {}): Conflict => ({
  path: 'a.txt',
  base: 'one\ntwo\nthree\n',
  ours: 'one\nOURS\nthree\n',
  theirs: 'one\nTHEIRS\nthree\n',
  ...over,
});

describe("a conflict's identity", () => {
  it('is the same for the same conflict, computed twice', async () => {
    expect(await conflictIdentity(conflict())).toBe(await conflictIdentity(conflict()));
  });

  it('is the same for the same conflict reached from a different object', async () => {
    const first = conflict();
    const second = {
      path: first.path,
      base: first.base,
      ours: first.ours,
      theirs: first.theirs,
    };

    expect(await conflictIdentity(second)).toBe(await conflictIdentity(first));
  });

  it.each([
    ["one side's content", { ours: 'one\nSOMETHING ELSE\nthree\n' }],
    ["the other side's content", { theirs: 'one\nSOMETHING ELSE\nthree\n' }],
    ['the base', { base: 'one\nDIFFERENT\nthree\n' }],
    ['the path', { path: 'b.txt' }],
  ])('differs when %s differs', async (_name, over) => {
    expect(await conflictIdentity(conflict(over))).not.toBe(await conflictIdentity(conflict()));
  });

  it('does not run two parts together, so moving a boundary changes it', async () => {
    const left = conflict({ ours: 'ab', theirs: 'c' });
    const right = conflict({ ours: 'a', theirs: 'bc' });

    expect(await conflictIdentity(left)).not.toBe(await conflictIdentity(right));
  });

  it.each([
    ['line endings', { ours: 'one\r\nOURS\r\nthree\r\n' }],
    ['trailing spaces', { ours: 'one   \nOURS\t\nthree\n' }],
  ])('is unchanged by %s, which do not change the conflict', async (_name, over) => {
    const normalised = conflict({
      ...over,
      ours: (over.ours ?? '').replaceAll('\r\n', '\n').replaceAll(/[ \t]+$/gm, ''),
    });

    expect(await conflictIdentity(conflict(over))).toBe(await conflictIdentity(normalised));
  });

  it('is a hex digest, so it can be a key anywhere', async () => {
    expect(await conflictIdentity(conflict())).toMatch(/^[0-9a-f]{64}$/);
  });

  it('is the same value this engine has always computed', async () => {
    // Written out, so a change to the identity is a change a reader
    // sees rather than one that quietly forgets every resolution.
    expect(await conflictIdentity({ path: 'a.txt', base: 'b', ours: 'o', theirs: 't' })).toBe(
      '8f976fde9571a4392320a220bbe4db9d6ebc2869ca6613b84358e1136d6b1a51',
    );
  });
});
