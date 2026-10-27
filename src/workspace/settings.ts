import { HttpError } from '../http/errors.js';

type SettingKind =
  | { kind: 'boolean'; fallback: boolean }
  | { kind: 'string'; fallback: string }
  | { kind: 'enum'; choices: readonly string[]; fallback: string };

/**
 * The behaviour switches of a workspace, transcribed from the API
 * reference's settings table. Every workspace object carries the effective
 * value of all of them, set or not.
 */
export const WORKSPACE_SETTINGS: Readonly<Record<string, SettingKind>> = {
  rebuild_strategy: { kind: 'enum', choices: ['rebase', 'merge'], fallback: 'rebase' },
  rebuild_fail_mode: { kind: 'enum', choices: ['fail_fast', 'continue'], fallback: 'fail_fast' },
  auto_rebuild_after_sync: { kind: 'boolean', fallback: true },
  auto_rebuild_after_push: { kind: 'boolean', fallback: true },
  patch_branch_source: { kind: 'enum', choices: ['hub', 'origin'], fallback: 'hub' },
  patch_divergence_policy: { kind: 'enum', choices: ['replace', 'report'], fallback: 'replace' },
  push_patches_to_origin: { kind: 'boolean', fallback: false },
  rebuild_push_integration_branch: { kind: 'boolean', fallback: false },
  squash_merge_detection: {
    kind: 'enum',
    choices: ['ancestry_only', 'content_based', 'both'],
    fallback: 'both',
  },
  forge_api_url: { kind: 'string', fallback: '' },
  upstream_forge_api_url: { kind: 'string', fallback: '' },
  sync_enabled: { kind: 'boolean', fallback: true },
};

export type SettingValue = string | boolean;

/** Every key at its effective value: what was set, else the default. */
export function effectiveSettings(stored: Record<string, unknown>): Record<string, SettingValue> {
  const effective: Record<string, SettingValue> = {};
  for (const [key, setting] of Object.entries(WORKSPACE_SETTINGS)) {
    const held = stored[key];
    effective[key] =
      held === undefined || held === null ? setting.fallback : (held as SettingValue);
  }
  return effective;
}

function checkedValue(key: string, setting: SettingKind, value: unknown): SettingValue {
  if (setting.kind === 'boolean') {
    if (typeof value !== 'boolean') {
      throw new HttpError(400, `${key} must be true or false`);
    }
    return value;
  }
  if (setting.kind === 'string') {
    if (typeof value !== 'string') {
      throw new HttpError(400, `${key} must be a string`);
    }
    return value;
  }
  if (typeof value !== 'string' || !setting.choices.includes(value)) {
    throw new HttpError(400, `${key} must be one of ${setting.choices.join(', ')}`);
  }
  return value;
}

/**
 * The settings to store, given what is stored now and what a write names. A
 * key named takes its value, a key named as null returns to its default by
 * being forgotten, and a key not named is kept.
 */
export function checkedSettings(
  given: unknown,
  stored: Record<string, unknown> = {},
): Record<string, unknown> {
  if (given === null) {
    return {};
  }
  if (given === undefined) {
    return { ...stored };
  }
  if (typeof given !== 'object' || Array.isArray(given)) {
    throw new HttpError(400, 'settings must be an object');
  }

  const next = { ...stored };
  for (const [key, value] of Object.entries(given as Record<string, unknown>)) {
    const setting = WORKSPACE_SETTINGS[key];
    if (setting === undefined) {
      throw new HttpError(
        400,
        `${key} is not a workspace setting; the settings are ${Object.keys(WORKSPACE_SETTINGS).join(', ')}`,
      );
    }
    if (value === null) {
      delete next[key];
      continue;
    }
    next[key] = checkedValue(key, setting, value);
  }
  return next;
}
