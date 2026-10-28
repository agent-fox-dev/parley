import { describe, expect, it } from 'vitest';
import { corpus } from './corpus.js';
import { compareOne } from '../testutil/corpusdiff.js';
import { hasRealGit } from '../testutil/realgit.js';

/**
 * Every case of the corpus, run through the engine and through the real
 * git binary. A difference in the tree, the commits produced, their
 * parents or the paths that conflicted fails the run and says which
 * case differed and how.
 */
const CASES = corpus();

describe.skipIf(!hasRealGit())(
  `the corpus against the real git binary${hasRealGit() ? '' : ' (needs git on the path)'}`,
  () => {
    it.each(CASES.map((one) => [one.name, one] as const))(
      'agrees with git on %s',
      async (name, one) => {
        expect(await compareOne(one), name).toEqual([]);
      },
      30_000,
    );
  },
);
