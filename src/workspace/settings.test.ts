import { describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import { checkedSettings, effectiveSettings } from './settings.js';

/** Written out from the reference, so a drift in the table is a failure here. */
const DOCUMENTED: Array<[string, unknown, readonly unknown[]]> = [
  ['rebuild_strategy', 'rebase', ['rebase', 'merge']],
  ['rebuild_fail_mode', 'fail_fast', ['fail_fast', 'continue']],
  ['auto_rebuild_after_sync', true, [true, false]],
  ['auto_rebuild_after_push', true, [true, false]],
  ['patch_branch_source', 'hub', ['hub', 'origin']],
  ['patch_divergence_policy', 'replace', ['replace', 'report']],
  ['push_patches_to_origin', false, [true, false]],
  ['rebuild_push_integration_branch', false, [true, false]],
  ['squash_merge_detection', 'both', ['ancestry_only', 'content_based', 'both']],
  ['forge_api_url', '', ['', 'https://api.github.com']],
  ['upstream_forge_api_url', '', ['', 'https://api.github.com']],
  ['sync_enabled', true, [true, false]],
];

function refusal(given: unknown, stored: Record<string, unknown> = {}): HttpError {
  try {
    checkedSettings(given, stored);
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the settings were accepted when they should have been refused');
}

describe('the settings of a workspace', () => {
  it('are exactly the twelve the reference lists', () => {
    expect(Object.keys(effectiveSettings({})).toSorted()).toEqual(
      DOCUMENTED.map(([key]) => key).toSorted(),
    );
  });

  it.each(DOCUMENTED)('%s reads its documented default when nothing set it', (key, fallback) => {
    expect(effectiveSettings({})[key as string]).toBe(fallback);
  });

  it.each(DOCUMENTED.flatMap(([key, , values]) => values.map((value) => [key, value])))(
    'accepts %s set to %s',
    (key, value) => {
      const next = checkedSettings({ [key as string]: value });

      expect(effectiveSettings(next)[key as string]).toBe(value);
    },
  );
});

describe('what an update does to the settings already stored', () => {
  const stored = { rebuild_strategy: 'merge', sync_enabled: false, patch_branch_source: 'origin' };

  it('takes the value of a key it names', () => {
    expect(
      effectiveSettings(checkedSettings({ rebuild_strategy: 'rebase' }, stored))[
        'rebuild_strategy'
      ],
    ).toBe('rebase');
  });

  it('returns a key given as null to its default', () => {
    expect(effectiveSettings(checkedSettings({ sync_enabled: null }, stored))['sync_enabled']).toBe(
      true,
    );
  });

  it('keeps a key it does not name', () => {
    const next = effectiveSettings(checkedSettings({ rebuild_strategy: 'rebase' }, stored));

    expect(next['sync_enabled']).toBe(false);
    expect(next['patch_branch_source']).toBe('origin');
  });

  it('returns every key to its default when the whole object is null', () => {
    const next = effectiveSettings(checkedSettings(null, stored));

    for (const [key, fallback] of DOCUMENTED) {
      expect(next[key as string]).toBe(fallback);
    }
  });

  it('keeps everything when no settings object is given at all', () => {
    expect(checkedSettings(undefined, stored)).toEqual(stored);
  });
});

describe('a setting the service refuses', () => {
  it('names the key when the key does not exist', () => {
    const err = refusal({ invented_switch: true });

    expect(err.status).toBe(400);
    expect(err.message).toContain('invented_switch');
  });

  it('names the key and the values it accepts when an enum value is wrong', () => {
    const err = refusal({ rebuild_strategy: 'squash' });

    expect(err.status).toBe(400);
    expect(err.message).toContain('rebuild_strategy');
    expect(err.message).toContain('rebase');
    expect(err.message).toContain('merge');
  });

  it.each([
    ['a boolean setting given a string', { sync_enabled: 'yes' }],
    ['a boolean setting given a number', { sync_enabled: 1 }],
    ['a string setting given a boolean', { forge_api_url: true }],
    ['an enum setting given a boolean', { patch_branch_source: true }],
  ])('refuses %s', (_name, given) => {
    expect(refusal(given).status).toBe(400);
  });

  it('changes nothing at all, including valid keys named in the same body', () => {
    const stored = { rebuild_strategy: 'merge' };

    refusal({ sync_enabled: false, rebuild_strategy: 'squash' }, stored);

    expect(stored).toEqual({ rebuild_strategy: 'merge' });
  });
});
