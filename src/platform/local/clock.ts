import type { Clock } from '../clock.js';

/** The host's own clock. Both targets read the time the same way. */
export function systemClock(): Clock {
  return { now: () => new Date() };
}

/** A clock that does not move, so a test can assert on an instant. */
export function fixedClock(at: Date): Clock {
  return { now: () => new Date(at.getTime()) };
}
