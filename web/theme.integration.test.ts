import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { renderDashboard, renderLanding, renderNotAvailable, renderWorkspace } from './render.js';
import { CONTRAST_NORMAL_TEXT, contrast, paletteOf } from '../src/testutil/contrast.js';
import { THEME_KEY } from './theme.js';

/**
 * Both themes, and whether a person can read them.
 *
 * The contrast is arithmetic over the palettes rather than a
 * screenshot: the rule is a ratio between two colours, and the
 * colours are in the token file. Every text colour is checked
 * against every surface it is put on, in both themes.
 */
const tokens = readFileSync('web/tokens.ts', 'utf8');

/** Every page the interface serves, rendered. */
const pages = (): Array<{ name: string; html: string }> => [
  { name: 'the landing page', html: renderLanding([{ name: 'github' }]) },
  {
    name: 'the dashboard',
    html: renderDashboard('alice', [
      { slug: 'widgets', status: 'ready', access: 'owner', last_sync_at: null },
    ]),
  },
  {
    name: 'a workspace',
    html: renderWorkspace('alice', {
      workspace: { slug: 'widgets', status: 'ready', workspace_mode: 'carry_patch' },
      patchStatus: { patches: [], last_rebuild: null },
      jobs: [],
      events: [],
    }),
  },
  { name: 'the not-available page', html: renderNotAvailable('alice') },
];

describe('the theme a page is read in', () => {
  it('is the viewer’s system setting until they choose, and their choice after', () => {
    // The stylesheet answers the system setting with a media query,
    // and the script applies a stored choice over it.
    expect(tokens).toContain('prefers-color-scheme: dark');
    expect(tokens).toContain(":root[data-theme='dark']");
    expect(tokens).toContain(":root:not([data-theme='light'])");
  });

  it('is applied by every page before the first paint, and remembered', () => {
    for (const { name, html } of pages()) {
      expect(html, `${name} applies a stored theme`).toContain(THEME_KEY);
      expect(html, `${name} applies it before the body`).toMatch(
        /<script[^>]*>[\s\S]*localStorage[\s\S]*<\/script>[\s\S]*<body/,
      );
      expect(html, `${name} offers the control`).toContain('data-theme-toggle');
    }
  });
});

describe('the colours of both themes', () => {
  it.each(['light', 'dark'] as const)('are all defined for the %s theme', (theme) => {
    const palette = paletteOf(tokens, theme);

    for (const name of ['--canvas', '--surface', '--fg', '--fg-muted', '--accent-fg']) {
      expect(palette[name], `${name} in the ${theme} palette`).toMatch(/^#[0-9a-f]{6}$/i);
    }
  });

  it.each(['light', 'dark'] as const)(
    'put every text colour on every surface readably in the %s theme',
    (theme) => {
      const palette = paletteOf(tokens, theme);
      const surfaces = ['--canvas', '--canvas-inset', '--surface'];
      const texts = [
        '--fg',
        '--fg-muted',
        '--accent-fg',
        '--success-fg',
        '--attention-fg',
        '--danger-fg',
      ];

      for (const surface of surfaces) {
        for (const text of texts) {
          const ratio = contrast(palette[text] as string, palette[surface] as string);
          expect(
            ratio,
            `${text} on ${surface} in the ${theme} theme is ${ratio.toFixed(2)}:1`,
          ).toBeGreaterThanOrEqual(CONTRAST_NORMAL_TEXT);
        }
      }
    },
  );

  it.each(['light', 'dark'] as const)(
    'put the text of an emphasis colour on it readably in the %s theme',
    (theme) => {
      const palette = paletteOf(tokens, theme);

      for (const emphasis of ['--success-emphasis', '--danger-emphasis', '--accent-emphasis']) {
        const ratio = contrast(palette['--on-emphasis'] as string, palette[emphasis] as string);
        expect(
          ratio,
          `${emphasis} in the ${theme} theme is ${ratio.toFixed(2)}:1`,
        ).toBeGreaterThanOrEqual(CONTRAST_NORMAL_TEXT);
      }
    },
  );
});
