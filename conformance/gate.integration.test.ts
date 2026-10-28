import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The fast gate must run on a laptop with no network and no deployment. It
 * is invoked here as a person invokes it, with both stripped from the
 * environment.
 *
 * The gate it invokes runs this suite again, so the inner run is marked and
 * skips itself. Without the mark the two would call each other for ever.
 */
const INNER = 'PARLEY_GATE_SELF_TEST';

describe.skipIf(process.env[INNER] === '1')('the fast gate', () => {
  it('exits 0 with no network access and no deployment configured', () => {
    const env = {
      ...process.env,
      [INNER]: '1',
      PARLEY_BASE_URL: '',
      HTTP_PROXY: 'http://127.0.0.1:1',
      HTTPS_PROXY: 'http://127.0.0.1:1',
      npm_config_registry: 'http://127.0.0.1:1',
    };

    const output = execFileSync('make', ['test'], {
      cwd: join(import.meta.dirname, '..'),
      env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 120_000,
    });

    expect(output).toContain('passed');
    expect(output).not.toContain('failed');
    // The inner run is the whole suite, so it needs the subprocess's own
    // budget rather than the default for a unit test.
  }, 180_000);
});
