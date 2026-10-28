import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

/**
 * The interface decides what anything looks like in one place. A
 * literal type size or spacing in a page is a value that will drift
 * from the rest the first time somebody changes either, so the
 * tokens are the only source and the pages are checked against it.
 */
const tokens = readFileSync('web/tokens.ts', 'utf8');

/** Every custom property the token file defines. */
function defined(): string[] {
  return [...tokens.matchAll(/^\s*(--[a-z0-9-]+):/gm)].map((match) => match[1] as string);
}

describe('the design tokens', () => {
  it('define the type scale, the spacing scale and the status colours', () => {
    const names = defined();

    for (const needed of [
      '--text-sm',
      '--text-base',
      '--text-lg',
      '--space-2',
      '--space-4',
      '--success-fg',
      '--attention-fg',
      '--danger-fg',
    ]) {
      expect(names, `${needed} is defined`).toContain(needed);
    }
  });

  it('define both palettes, so a viewer’s system setting has one to pick', () => {
    expect(tokens).toContain('prefers-color-scheme: dark');
    // The same colour names in both, so no page has to know which
    // theme it is being read in.
    const light = tokens.slice(tokens.indexOf(':root {'), tokens.indexOf('@media'));
    const dark = tokens.slice(tokens.indexOf('@media'));
    for (const name of ['--canvas', '--fg', '--fg-muted', '--border', '--accent-fg']) {
      expect(light, `${name} in the light palette`).toContain(`${name}:`);
      expect(dark, `${name} in the dark palette`).toContain(`${name}:`);
    }
  });
});

/** Every component and page the interface renders. */
function rendered(): Array<{ path: string; source: string }> {
  return execFileSync('git', ['ls-files', 'web'], { encoding: 'utf8' })
    .split('\n')
    .filter((path) => path.endsWith('.tsx'))
    .map((path) => ({ path, source: readFileSync(path, 'utf8') }));
}

describe('the pages and the patterns', () => {
  it('set no type size or spacing of their own', () => {
    const literals =
      /style\s*=\s*[{"'][^}"']*(?:font-size|padding|margin|gap|line-height)\s*:\s*(?!var\()/i;

    const offenders = rendered()
      .filter((file) => literals.test(file.source))
      .map((file) => file.path);

    expect(offenders).toEqual([]);
  });

  it('draw their colours from the tokens rather than naming one', () => {
    // A hex colour in a page is a colour that will not change with
    // the theme, which is the one thing both themes need.
    const offenders = rendered()
      .filter((file) => /#[0-9a-f]{3,8}\b/i.test(file.source))
      .map((file) => file.path);

    expect(offenders).toEqual([]);
  });
});
