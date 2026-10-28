import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import type { CorpusCase } from '../history/corpus.js';
import type { Snapshot } from './history.js';

export interface CaseOutcome {
  conflicts?: string[];
  /** The tree the operation ended at. */
  tree?: string;
  /** The messages of the commits the operation produced, oldest first. */
  messages?: string[];
  /** How many parents each produced commit has, in the same order. */
  parentCounts?: number[];
}

/** Runs one corpus case with the real git binary. */
export function realGitCase(one: CorpusCase): CaseOutcome {
  const dir = mkdtempSync(join(tmpdir(), 'parley-corpus-'));
  const run = (...args: string[]) =>
    execFileSync('git', args, {
      cwd: dir,
      encoding: 'utf8',
      env: {
        ...process.env,
        GIT_AUTHOR_NAME: 'Fixture',
        GIT_AUTHOR_EMAIL: 'fixture@parley.test',
        GIT_COMMITTER_NAME: 'Parley',
        GIT_COMMITTER_EMAIL: 'parley@agent-fox.dev',
        GIT_AUTHOR_DATE: '2023-11-14T22:13:20Z',
        GIT_COMMITTER_DATE: '2027-01-15T08:00:00Z',
        GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_CONFIG_SYSTEM: '/dev/null',
      },
    });

  function write(files: Snapshot): void {
    for (const entry of run('ls-files').split('\n').filter(Boolean)) {
      rmSync(join(dir, entry), { force: true });
    }
    run('rm', '-rq', '--cached', '--ignore-unmatch', '.');
    for (const [path, content] of Object.entries(files)) {
      mkdirSync(dirname(join(dir, path)), { recursive: true });
      writeFileSync(join(dir, path), content);
      run('add', '--', path);
    }
  }

  function commits(sets: Snapshot[], prefix: string): void {
    for (const [index, files] of sets.entries()) {
      write(files);
      run('commit', '-q', '-m', `${prefix} ${String(index + 1)}`, '--allow-empty');
    }
  }

  try {
    run('init', '-q', '-b', 'main');
    run('config', 'user.name', 'Fixture');
    run('config', 'user.email', 'fixture@parley.test');
    run('config', 'merge.conflictStyle', 'merge');
    write(one.root);
    run('commit', '-q', '-m', 'root', '--allow-empty');
    run('branch', 'topic');

    commits(one.base, 'base');
    run('checkout', '-q', 'topic');
    commits(one.branch, 'branch');

    if (one.crossOver === true) {
      run('merge', '--no-edit', '-m', 'cross into topic', 'main');
      run('checkout', '-q', 'main');
      run('merge', '--no-edit', '-m', 'cross into main', 'topic~1');
      run('checkout', '-q', 'topic');
    }

    if (one.operation === 'merge') {
      run('checkout', '-q', 'main');
      try {
        run('merge', '--no-edit', '-m', 'merge', 'topic');
      } catch {
        return { conflicts: conflictsIn(run) };
      }
      return {
        tree: run('rev-parse', 'HEAD^{tree}').trim(),
        messages: ['merge'],
        parentCounts: [
          run('rev-list', '--parents', '-n', '1', 'HEAD').trim().split(' ').length - 1,
        ],
      };
    }

    try {
      run('rebase', 'main');
    } catch {
      const conflicts = conflictsIn(run);
      run('rebase', '--abort');
      return { conflicts };
    }
    const messages = run('log', '--format=%s', 'main..HEAD')
      .split('\n')
      .filter(Boolean)
      .toReversed();
    return {
      tree: run('rev-parse', 'HEAD^{tree}').trim(),
      messages,
      parentCounts: messages.map(() => 1),
    };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

function conflictsIn(run: (...args: string[]) => string): string[] {
  return run('diff', '--name-only', '--diff-filter=U').split('\n').filter(Boolean).toSorted();
}
