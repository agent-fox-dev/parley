import { describe, expect, it } from 'vitest';
import { corpus } from './corpus.js';
import { engineCase } from '../testutil/corpusengine.js';
import { realGitCase } from '../testutil/corpusgit.js';
import { hasRealGit } from '../testutil/realgit.js';

/**
 * Every case of the corpus, run through the engine and through the real
 * git binary. A difference in the tree, the commits produced, their
 * parents or the paths that conflicted fails the run and says which
 * case differed.
 */
const CASES = corpus();

describe.skipIf(!hasRealGit())(
  `the corpus against the real git binary${hasRealGit() ? '' : ' (needs git on the path)'}`,
  () => {
    it.each(CASES.map((one) => [one.name, one] as const))(
      'agrees with git on %s',
      async (name, one) => {
        const mine = await engineCase(one);
        const real = realGitCase(one);

        expect(mine.conflicts ?? [], `${name}: the paths that conflicted`).toEqual(
          real.conflicts ?? [],
        );
        if (real.conflicts !== undefined) {
          return;
        }

        expect(mine.tree, `${name}: the resulting tree`).toBe(real.tree);
        expect(mine.messages, `${name}: the commits produced, in order`).toEqual(real.messages);
        expect(mine.parentCounts, `${name}: each commit's parents`).toEqual(real.parentCounts);
      },
      30_000,
    );
  },
);
