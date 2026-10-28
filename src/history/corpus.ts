import type { Snapshot } from '../testutil/history.js';

/** The numbers below a count, in order. */
function upTo(count: number): number[] {
  return Array.from({ length: count }, (_, i) => i);
}

/** The shapes of history the corpus has to cover. */
export type Shape =
  | 'linear'
  | 'branching'
  | 'rename'
  | 'binary'
  | 'mode'
  | 'empty-commit'
  | 'merge-in-source'
  | 'conflicting'
  | 'clean'
  | 'deep-stack';

export interface CorpusCase {
  name: string;
  shapes: Shape[];
  /** The commit both sides start from. */
  root: Snapshot;
  /** The commits the base branch adds, in order. */
  base: Snapshot[];
  /** The commits the other branch adds, in order. */
  branch: Snapshot[];
  /** Whether the branch is merged into the base or replayed onto it. */
  operation: 'merge' | 'rebase';
  /** Whether the branch merges the base in part way, crossing the two. */
  crossOver?: boolean;
}

/**
 * A small deterministic generator, so a corpus case that fails is the
 * same case on the next run and can be looked at.
 */
function sequence(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state * 1_664_525 + 1_013_904_223) >>> 0;
    return state / 0x1_0000_0000;
  };
}

const WORDS = ['alpha', 'beta', 'gamma', 'delta', 'epsilon', 'zeta', 'eta', 'theta'];

function lines(count: number, next: () => number): string {
  return (
    Array.from({ length: count }, (_, i) => {
      const word = WORDS[Math.floor(next() * WORDS.length)] ?? 'alpha';
      return `${String(i)} ${word}`;
    }).join('\n') + '\n'
  );
}

/** Bytes no line merge should ever be attempted on. */
function binary(seed: string): string {
  return `\u0000\u0001${seed}\u0000ÿ\n`;
}

/**
 * The corpus: every shape the package names, generated rather than
 * written out, so a change that diverges from git fails on a case
 * nobody thought to write by hand.
 */
export function corpus(): CorpusCase[] {
  const cases: CorpusCase[] = [];
  const next = sequence(20_261_028);

  // Linear histories of growing length, merged and rebased.
  for (let depth = 1; depth <= 6; depth += 1) {
    for (const operation of ['merge', 'rebase'] as const) {
      const root: Snapshot = { 'a.txt': lines(6, next) };
      cases.push({
        name: `linear depth ${String(depth)}, ${operation}`,
        shapes: ['linear', 'clean'],
        root,
        base: [{ ...root, 'base.txt': lines(3, next) }],
        branch: upTo(depth).map((reached) => ({
          ...root,
          ...Object.fromEntries(
            upTo(reached + 1).map((j) => [`branch-${String(j)}.txt`, lines(2, next)]),
          ),
        })),
        operation,
      });
    }
  }

  // Each side edits its own region of one file: clean, and the merge
  // has to interleave the two.
  for (let i = 0; i < 20; i += 1) {
    const body = lines(12, next).split('\n');
    const ours = [...body];
    const theirs = [...body];
    ours[1] = `1 OURS ${String(i)}`;
    theirs[9] = `9 THEIRS ${String(i)}`;
    cases.push({
      name: `separate regions ${String(i)}`,
      shapes: ['clean', 'linear'],
      root: { 'a.txt': body.join('\n') },
      base: [{ 'a.txt': ours.join('\n') }],
      branch: [{ 'a.txt': theirs.join('\n') }],
      operation: i % 2 === 0 ? 'merge' : 'rebase',
    });
  }

  // The same region on both sides: a conflict every time.
  for (let i = 0; i < 20; i += 1) {
    const body = lines(8, next).split('\n');
    const ours = [...body];
    const theirs = [...body];
    ours[4] = `4 OURS ${String(i)}`;
    theirs[4] = `4 THEIRS ${String(i)}`;
    cases.push({
      name: `same region ${String(i)}`,
      shapes: ['conflicting'],
      root: { 'a.txt': body.join('\n') },
      base: [{ 'a.txt': ours.join('\n') }],
      branch: [{ 'a.txt': theirs.join('\n') }],
      operation: i % 2 === 0 ? 'merge' : 'rebase',
    });
  }

  // Renames: one side moves a file, the other leaves it or edits it.
  for (let i = 0; i < 16; i += 1) {
    const content = lines(5, next);
    const edited = i % 2 === 0;
    // One quiet path neither side touches, so the only thing in play is
    // the rename itself.
    const quiet = lines(2, next);
    cases.push({
      name: `rename ${String(i)}${edited ? ' against an edit' : ''}`,
      shapes: ['rename', 'clean'],
      root: { 'old.txt': content, 'other.txt': quiet },
      base: [{ 'new.txt': content, 'other.txt': quiet }],
      branch: [
        edited
          ? { 'old.txt': `${content}one more line\n`, 'other.txt': quiet }
          : { 'old.txt': content, 'other.txt': quiet, 'added.txt': lines(2, next) },
      ],
      operation: i % 3 === 0 ? 'rebase' : 'merge',
    });
  }

  // Binary files, changed on one side and on both.
  for (let i = 0; i < 16; i += 1) {
    const both = i % 2 === 0;
    cases.push({
      name: `binary ${String(i)}${both ? ' on both sides' : ''}`,
      shapes: ['binary', both ? 'conflicting' : 'clean'],
      root: { 'blob.bin': binary('base'), 'a.txt': lines(3, next) },
      base: [{ 'blob.bin': binary(`ours-${String(i)}`), 'a.txt': lines(3, next) }],
      branch: [
        {
          'blob.bin': both ? binary(`theirs-${String(i)}`) : binary('base'),
          'a.txt': lines(3, next),
        },
      ],
      operation: 'merge',
    });
  }

  // Commits that change nothing, which git drops on a rebase.
  for (let i = 0; i < 12; i += 1) {
    const root: Snapshot = { 'a.txt': lines(4, next) };
    const shared: Snapshot = { ...root, 'shared.txt': 'both added this\n' };
    cases.push({
      name: `empty commit ${String(i)}`,
      shapes: ['empty-commit', 'linear'],
      root,
      base: [shared],
      branch: [shared, { ...shared, 'own.txt': lines(2, next) }],
      operation: i % 2 === 0 ? 'rebase' : 'merge',
    });
  }

  // Histories that crossed over, so the two sides share two bases.
  for (let i = 0; i < 12; i += 1) {
    const root: Snapshot = { 'a.txt': lines(5, next) };
    cases.push({
      name: `merge in the source ${String(i)}`,
      shapes: ['merge-in-source', 'branching', 'clean'],
      root,
      base: [
        { ...root, 'b.txt': lines(2, next) },
        { ...root, 'b.txt': lines(2, next), 'b2.txt': lines(2, next) },
      ],
      branch: [
        { ...root, 'c.txt': lines(2, next) },
        { ...root, 'c.txt': lines(2, next), 'c2.txt': lines(2, next) },
      ],
      operation: 'merge',
      crossOver: true,
    });
  }

  // Branching histories: several files moving at once on both sides.
  for (let i = 0; i < 56; i += 1) {
    const paths = upTo(4).map((j) => `f${String(j)}.txt`);
    const root = Object.fromEntries(paths.map((path) => [path, lines(4, next)]));
    const ours = { ...root };
    const theirs = { ...root };
    for (const [index, path] of paths.entries()) {
      if ((i + index) % 2 === 0) {
        ours[path] = lines(4, next);
      } else {
        theirs[path] = lines(4, next);
      }
    }
    cases.push({
      name: `branching ${String(i)}`,
      shapes: ['branching', 'clean'],
      root,
      base: [ours],
      branch: [theirs],
      operation: i % 2 === 0 ? 'merge' : 'rebase',
    });
  }

  // Deletions against edits, and deletions on both sides.
  for (let i = 0; i < 20; i += 1) {
    const root: Snapshot = { 'a.txt': lines(4, next), 'doomed.txt': lines(3, next) };
    const bothDelete = i % 3 === 0;
    cases.push({
      name: `deletion ${String(i)}`,
      shapes: [bothDelete ? 'clean' : 'conflicting'],
      root,
      base: [{ 'a.txt': root['a.txt'] ?? '' }],
      branch: [
        bothDelete
          ? { 'a.txt': root['a.txt'] ?? '' }
          : { 'a.txt': root['a.txt'] ?? '', 'doomed.txt': `${root['doomed.txt'] ?? ''}edited\n` },
      ],
      operation: 'merge',
    });
  }

  // A stack of patches deep enough to be worth calling a stack.
  for (const depth of [20, 24, 28]) {
    const root: Snapshot = { 'a.txt': lines(6, next) };
    let carried: Snapshot = { ...root };
    const branch: Snapshot[] = [];
    for (let i = 0; i < depth; i += 1) {
      carried = { ...carried, [`patch-${String(i)}.txt`]: lines(2, next) };
      branch.push(carried);
    }
    cases.push({
      name: `a stack of ${String(depth)} patches`,
      shapes: ['deep-stack', 'linear', 'clean'],
      root,
      base: [{ ...root, 'base.txt': lines(2, next) }],
      branch,
      operation: 'rebase',
    });
  }

  // Mode changes: one side makes a file executable.
  for (let i = 0; i < 16; i += 1) {
    const content = lines(4, next);
    cases.push({
      name: `mode change ${String(i)}`,
      shapes: ['mode', 'clean'],
      root: { 'script.sh': content, 'a.txt': lines(2, next) },
      base: [{ 'script.sh': content, 'a.txt': lines(2, next), 'added.txt': lines(2, next) }],
      branch: [{ 'script.sh': content, 'a.txt': lines(3, next) }],
      operation: i % 2 === 0 ? 'merge' : 'rebase',
    });
  }

  return cases;
}

/** Every shape the corpus is required to cover. */
export const REQUIRED_SHAPES: Shape[] = [
  'linear',
  'branching',
  'rename',
  'binary',
  'mode',
  'empty-commit',
  'merge-in-source',
  'conflicting',
  'clean',
  'deep-stack',
];
