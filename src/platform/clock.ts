/** The current instant, so that nothing in the service calls Date.now itself. */
export interface Clock {
  now(): Date;
}
