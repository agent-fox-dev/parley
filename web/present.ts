import type { BadgeTone } from './components/patterns.js';

/**
 * Turning what the API says into what a page shows.
 *
 * It is one module so that a status reads the same on every page,
 * and so that "never" is said rather than left as an empty cell.
 */
const TONES: Readonly<Record<string, BadgeTone>> = {
  ready: 'success',
  active: 'success',
  completed: 'success',
  pending: 'attention',
  cloning: 'attention',
  queued: 'attention',
  running: 'attention',
  conflict: 'danger',
  failed: 'danger',
  error: 'danger',
};

/** A status, in the colour its state earns. */
export function badgeFor(status: string): { text: string; tone: BadgeTone } {
  return { text: status, tone: TONES[status] ?? 'neutral' };
}

/** A time as a person reads one, and "never" where there is none. */
export function saidWhen(at: string | null | undefined): string {
  if (at === null || at === undefined || at.length === 0) {
    return 'never';
  }
  const parsed = Date.parse(at);
  return Number.isNaN(parsed)
    ? 'never'
    : new Date(parsed)
        .toISOString()
        .replace('T', ' ')
        .replace(/\.\d+Z$/, 'Z');
}
