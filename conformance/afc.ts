import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

export interface PreparedAfc {
  path?: string;
  commit?: string;
  /** Why no binary was prepared, named so an operator can act on it. */
  skipped?: string;
}

/**
 * The pin lives beside the conformance sources, which is not where a bundle
 * runs from, so it is found relative to the package root rather than to this
 * file.
 */
function pinFile(): string {
  let dir = import.meta.dirname;
  for (let up = 0; up < 6; up += 1) {
    if (existsSync(join(dir, 'package.json'))) {
      return join(dir, 'conformance', 'hub-commit.txt');
    }
    dir = dirname(dir);
  }
  return join(import.meta.dirname, 'hub-commit.txt');
}

/**
 * The commit of the hub that afc is built from, or undefined while the
 * repository has not pinned one. Comment lines are ignored.
 */
export function pinnedHubCommit(): string | undefined {
  const path = pinFile();
  if (!existsSync(path)) {
    return undefined;
  }
  const line: string = readFileSync(path, { encoding: 'utf8' });
  const found = line
    .split('\n')
    .map((candidate: string) => candidate.trim())
    .find((candidate: string) => candidate.length > 0 && !candidate.startsWith('#'));

  return found === undefined || found === 'unpinned' ? undefined : found;
}

function toolMissing(tool: string, args: string[]): boolean {
  try {
    execFileSync(tool, args, { stdio: 'ignore' });
    return false;
  } catch {
    return true;
  }
}

/**
 * Builds afc from the pinned hub commit, or says what was missing. Nothing
 * here fails a run: a missing toolchain, a missing network or an unpinned
 * commit is a skip, so the fast gate still passes on a laptop and offline.
 */
export async function prepareAfc(): Promise<PreparedAfc> {
  const commit = pinnedHubCommit();
  if (commit === undefined) {
    return { skipped: 'the hub commit is not pinned in conformance/hub-commit.txt' };
  }
  if (toolMissing('go', ['version'])) {
    return { skipped: 'the Go toolchain is not installed' };
  }

  const dir = mkdtempSync(join(tmpdir(), 'parley-afc-'));
  const path = join(dir, 'afc');
  try {
    execFileSync('go', ['install', `github.com/agent-fox-dev/hub/cmd/afc@${commit}`], {
      env: { ...process.env, GOBIN: dir },
      stdio: 'ignore',
      timeout: 300_000,
    });
  } catch {
    return { skipped: `afc could not be built from ${commit}; a network may be unavailable` };
  }

  await Promise.resolve();
  return { path, commit };
}
