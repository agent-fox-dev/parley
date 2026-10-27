/** Where the service reads its settings, whatever supplies them. */
export interface ConfigSource {
  /** The setting's value, or undefined where it is not set or is empty. */
  get(key: string): string | undefined;
}
