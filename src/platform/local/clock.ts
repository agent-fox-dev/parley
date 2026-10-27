import type { Clock } from '../clock.js';

/** The host's own clock. Both targets read the time the same way. */
export function systemClock(): Clock {
  return { now: () => new Date() };
}

/** A clock that does not move, so a test can assert on an instant. */
export function fixedClock(at: Date): Clock {
  return { now: () => new Date(at.getTime()) };
}

/**
 * A clock a test moves itself, for behaviour that turns on time passing
 * without a test having to wait for it.
 */
export function movableClock(start: Date): { clock: Clock; advance: (ms: number) => void } {
  let at = start.getTime();
  return {
    clock: { now: () => new Date(at) },
    advance: (ms: number) => {
      at += ms;
    },
  };
}
