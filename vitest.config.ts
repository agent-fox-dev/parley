import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    // Unit and integration suites are hermetic and run in the fast gate.
    // Smoke suites need a running deployment and run in `make check-release`.
    include: ['src/**/*.test.ts'],
    exclude: ['src/**/*.smoke.test.ts'],
    environment: 'node',
  },
});
