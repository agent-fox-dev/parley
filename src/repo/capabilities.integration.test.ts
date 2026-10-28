import { describe, expect, it } from 'vitest';
import { ARTIFACTS_CAPABILITIES } from './capabilities.js';

/**
 * Holds the declared capabilities against the store that is supposed to
 * have them. The declaration was established by experiment (ADR 6); this
 * is what notices if a later version of the store withdraws one.
 *
 * It needs a repository to ask, so it skips without one. `make conformance`
 * is where a deployment supplies it.
 */
const remote = (process.env['PARLEY_ARTIFACTS_REMOTE'] ?? '').replace(/\/$/, '');
const token = process.env['PARLEY_ARTIFACTS_TOKEN'] ?? '';

const absent = [
  ['PARLEY_ARTIFACTS_REMOTE', remote],
  ['PARLEY_ARTIFACTS_TOKEN', token],
]
  .filter(([, value]) => (value ?? '').length === 0)
  .map(([name]) => name)
  .join(', ');

/** The capability list a smart HTTP advertisement carries, as pkt-lines. */
async function advertised(service: string, version?: string): Promise<string> {
  const response = await fetch(`${remote}/info/refs?service=${service}`, {
    headers: {
      authorization: `Bearer ${token}`,
      ...(version === undefined ? {} : { 'git-protocol': version }),
    },
  });
  expect(response.status).toBe(200);
  return response.text();
}

describe.skipIf(absent.length > 0)(
  `the capabilities the store really has${absent.length > 0 ? ` (needs ${absent})` : ''}`,
  () => {
    it('advertises deletion exactly where the declaration says it has it', async () => {
      const advertisement = await advertised('git-receive-pack');

      expect(advertisement.includes('delete-refs')).toBe(ARTIFACTS_CAPABILITIES.refDeletion);
    });

    it('speaks protocol v2, so a copy can ask only for what it needs', async () => {
      const advertisement = await advertised('git-upload-pack', 'version=2');

      expect(advertisement).toContain('version 2');
      expect(advertisement).toContain('ls-refs');
      expect(advertisement).toContain('fetch=');
    });

    it('serves a ref listing the service can read', async () => {
      const advertisement = await advertised('git-upload-pack');

      // The advertisement is pkt-lines, so it starts with a four-digit
      // length and names the service it is answering for.
      expect(advertisement).toMatch(/^[0-9a-f]{4}# service=git-upload-pack/);
    });
  },
);
