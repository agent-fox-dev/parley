import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { renderDashboard, renderLanding, renderNotAvailable, renderWorkspace } from './render.js';

/**
 * Whether a page fits a phone.
 *
 * What can be measured without a browser is measured here: that
 * every page asks the browser to use the device's width, that
 * nothing in the stylesheet is wider than the narrowest viewport
 * the package promises, and that the elements carrying a slug or a
 * description are told to wrap. Whether the rendered layout really
 * holds at 360 pixels is a browser's answer, and it is the smoke
 * tests that ask for it.
 */
const tokens = readFileSync('web/tokens.ts', 'utf8');

/** The narrowest viewport every page must work at. */
const NARROWEST = 360;

const LONG_SLUG = 'a-workspace-with-an-unreasonably-long-name-that-somebody-will-certainly-create';
const LONG_DESCRIPTION =
  'A description somebody pasted a whole paragraph into, which must wrap rather than widen the page beyond the viewport it is being read in.';

const pages = (): Array<{ name: string; html: string }> => [
  { name: 'the landing page', html: renderLanding([{ name: 'github' }]) },
  {
    name: 'the dashboard',
    html: renderDashboard('alice', [
      { slug: 'widgets', status: 'ready', access: 'owner', last_sync_at: null },
      { slug: LONG_SLUG, status: 'cloning', access: 'member', last_sync_at: null },
    ]),
  },
  {
    name: 'a workspace',
    html: renderWorkspace('alice', {
      workspace: {
        slug: LONG_SLUG,
        status: 'ready',
        workspace_mode: 'standard',
        description: LONG_DESCRIPTION,
        head_sha: 'a'.repeat(40),
        settings: { sync_enabled: true },
      },
      jobs: [],
      events: [],
    }),
  },
  { name: 'the not-available page', html: renderNotAvailable('alice') },
];

describe('every page on a phone', () => {
  it('asks the browser to use the device’s own width', () => {
    for (const { name, html } of pages()) {
      expect(html, name).toContain('name="viewport"');
      expect(html, name).toContain('width=device-width');
    }
  });

  it('is laid out by a stylesheet with nothing fixed wider than the viewport', () => {
    // A max-width is a ceiling and fine, and so is a token naming
    // one; a plain fixed width is not.
    const fixed = [...tokens.matchAll(/(?:^|[;{\s])width:\s*(\d+)px/gm)].map((match) =>
      Number(match[1]),
    );

    expect(fixed.filter((px) => px > NARROWEST)).toEqual([]);
  });

  it('tells the elements that carry long values to wrap', () => {
    for (const selector of ['.repo-list .repo-name', '.fields dd']) {
      const from = tokens.indexOf(selector);
      expect(from, `${selector} is styled`).toBeGreaterThan(-1);
      const block = tokens.slice(from, tokens.indexOf('}', from));
      expect(block, `${selector} wraps`).toContain('overflow-wrap: anywhere');
    }
  });

  it('puts a long slug and a long description inside those elements', () => {
    const [, dashboard, workspace] = pages();

    expect(dashboard?.html).toContain(`<div class="repo-name">`);
    expect(dashboard?.html).toContain(LONG_SLUG);
    expect(workspace?.html).toContain(LONG_DESCRIPTION);
  });

  it('stacks the field grid at a phone’s width rather than keeping two columns', () => {
    const at = tokens.indexOf('@media (max-width: 480px)');

    expect(at).toBeGreaterThan(-1);
    expect(tokens.slice(at, at + 200)).toContain('grid-template-columns: 1fr');
  });
});
