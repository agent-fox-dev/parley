import { execFileSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';
import { pinnedHubCommit, prepareAfc } from './afc.js';

function goMissing(): boolean {
  try {
    execFileSync('go', ['version'], { stdio: 'ignore' });
    return false;
  } catch {
    return true;
  }
}

describe('the pinned hub commit', () => {
  it('is either a commit id or explicitly unpinned', () => {
    const pin = pinnedHubCommit();

    if (pin !== undefined) {
      expect(pin).toMatch(/^[0-9a-f]{7,40}$/);
    }
  });
});

describe('preparing afc', () => {
  it('builds from the pinned commit, or skips with a reason naming what was missing', async () => {
    const prepared = await prepareAfc();

    if (prepared.skipped !== undefined) {
      expect(prepared.skipped).toMatch(/not pinned|Go toolchain|network/);
      expect(prepared.path).toBeUndefined();
      return;
    }

    expect(prepared.path).toBeDefined();
    expect(prepared.commit).toBe(pinnedHubCommit());
  });

  it('skips naming the pin while the repository pins no commit', async () => {
    if (pinnedHubCommit() !== undefined) {
      return;
    }

    const prepared = await prepareAfc();

    expect(prepared.skipped).toContain('not pinned');
    expect(prepared.skipped).toContain('conformance/hub-commit.txt');
  });

  it('skips naming the toolchain when Go is absent and a commit is pinned', async () => {
    if (pinnedHubCommit() === undefined || !goMissing()) {
      return;
    }

    expect((await prepareAfc()).skipped).toContain('Go toolchain');
  });
});
