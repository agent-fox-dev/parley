import { defineConfig } from 'vitest/config';

/** The suites that need a running deployment. Run by `make test-smoke`. */
export default defineConfig({
  test: {
    include: ['src/**/*.smoke.test.ts', 'conformance/**/*.smoke.test.ts', 'web/**/*.smoke.test.ts'],
    environment: 'node',
    testTimeout: 60_000,
  },
});
