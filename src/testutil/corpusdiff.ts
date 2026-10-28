import type { CorpusCase } from '../history/corpus.js';
import { engineCase } from './corpusengine.js';
import { realGitCase } from './corpusgit.js';

/** One way a case came out differently for the engine than for git. */
export interface Difference {
  case: string;
  what: string;
  engine: unknown;
  git: unknown;
}

/** What a run over the whole corpus found. */
export interface CorpusReport {
  compared: number;
  mismatches: number;
  differences: Difference[];
}

/**
 * One case, run through the engine and through the real git binary.
 * The answer is every way the two differed, so an empty list is
 * agreement.
 */
export async function compareOne(one: CorpusCase): Promise<Difference[]> {
  const engine = await engineCase(one);
  const real = realGitCase(one);
  const found: Difference[] = [];
  const differs = (what: string, mine: unknown, theirs: unknown) => {
    if (JSON.stringify(mine) !== JSON.stringify(theirs)) {
      found.push({ case: one.name, what, engine: mine, git: theirs });
    }
  };

  differs('the paths that conflicted', engine.conflicts ?? [], real.conflicts ?? []);
  if (real.conflicts !== undefined || found.length > 0) {
    return found;
  }

  differs('the resulting tree', engine.tree, real.tree);
  differs('the commits produced, in order', engine.messages, real.messages);
  differs("each commit's parents", engine.parentCounts, real.parentCounts);
  return found;
}

/** Every case of a corpus, compared, with a count of what disagreed. */
export async function compareCorpus(cases: readonly CorpusCase[]): Promise<CorpusReport> {
  const differences: Difference[] = [];
  for (const one of cases) {
    differences.push(...(await compareOne(one)));
  }
  return {
    compared: cases.length,
    mismatches: new Set(differences.map((one) => one.case)).size,
    differences,
  };
}
