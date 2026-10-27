import type { ConfigSource } from '../config.js';

/** The settings a Worker is deployed with, read from its bindings. */
export function envConfig(env: Record<string, unknown>): ConfigSource {
  return {
    get(key: string): string | undefined {
      const value = env[key];
      return typeof value === 'string' && value.length > 0 ? value : undefined;
    },
  };
}
