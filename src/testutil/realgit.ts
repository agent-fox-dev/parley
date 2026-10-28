import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

/** What a commit holds: a path to its content, bytes or text. */
export type Files = Record<string, string | Uint8Array>;

export interface GitMerge {
  conflicts?: string[];
  /** The merged content, path to text, when it merged cleanly. */
  files?: Record<string, string>;
}

/** Whether a git binary can be found and run. */
export function hasRealGit(): boolean {
  try {
    execFileSync('git', ['--version'], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

/**
 * Runs the same three-way merge the engine is asked for, with the real
 * git binary, so the two can be compared. Modes are given as a map from
 * path to git mode where a case needs one.
 */
export function realGitMerge(
  base: Files,
  ours: Files,
  theirs: Files,
  modes: Record<string, string> = {},
): GitMerge {
  const dir = mkdtempSync(join(tmpdir(), 'parley-realgit-'));
  const run = (...args: string[]) =>
    execFileSync('git', args, {
      cwd: dir,
      encoding: 'utf8',
      env: {
        ...process.env,
        GIT_AUTHOR_NAME: 'Fixture',
        GIT_AUTHOR_EMAIL: 'fixture@parley.test',
        GIT_COMMITTER_NAME: 'Fixture',
        GIT_COMMITTER_EMAIL: 'fixture@parley.test',
        GIT_AUTHOR_DATE: '2023-11-14T22:13:20Z',
        GIT_COMMITTER_DATE: '2023-11-14T22:13:20Z',
      },
    });

  function write(files: Files): void {
    // Listed before anything is removed: clearing the index first would
    // leave the old files on disk, untracked, and in the way.
    for (const entry of run('ls-files').split('\n').filter(Boolean)) {
      rmSync(join(dir, entry), { force: true });
    }
    run('rm', '-rq', '--cached', '--ignore-unmatch', '.');
    for (const [path, content] of Object.entries(files)) {
      mkdirSync(dirname(join(dir, path)), { recursive: true });
      writeFileSync(join(dir, path), content);
      run('add', '--', path);
      const mode = modes[path];
      if (mode !== undefined) {
        run('update-index', '--chmod=+x', '--', path);
        void mode;
      }
    }
  }

  try {
    run('init', '-q', '-b', 'main');
    run('config', 'user.name', 'Fixture');
    run('config', 'user.email', 'fixture@parley.test');

    write(base);
    run('commit', '-q', '-m', 'base', '--allow-empty');
    run('branch', 'theirs');

    write(ours);
    run('commit', '-q', '-m', 'ours', '--allow-empty');

    run('checkout', '-q', 'theirs');
    write(theirs);
    run('commit', '-q', '-m', 'theirs', '--allow-empty');

    run('checkout', '-q', 'main');
    try {
      run('merge', '--no-edit', 'theirs');
    } catch {
      const conflicts = run('diff', '--name-only', '--diff-filter=U')
        .split('\n')
        .filter(Boolean)
        .toSorted();
      return { conflicts };
    }

    const files: Record<string, string> = {};
    for (const path of run('ls-files').split('\n').filter(Boolean)) {
      files[path] = run('show', `HEAD:${path}`);
    }
    return { files };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
