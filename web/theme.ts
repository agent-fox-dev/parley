/**
 * Which theme a page is read in.
 *
 * The default is the viewer's own system setting, which the
 * stylesheet answers with a media query and no script at all. A
 * viewer who wants the other one says so, and that choice is
 * remembered in their browser and applied before the first paint —
 * otherwise every page would flash the system's theme first.
 */
export type Theme = 'dark' | 'light';

/** Where the choice is kept, in the viewer's own browser. */
export const THEME_KEY = 'parley-theme';

/** The theme a stored value names, or none where it names neither. */
export function chosenTheme(stored: string | null | undefined): Theme | undefined {
  return stored === 'dark' || stored === 'light' ? stored : undefined;
}

/**
 * What the document's `data-theme` should be: the choice where there
 * is one, and nothing at all where there is none, so the stylesheet's
 * media query decides.
 */
export function themeAttribute(stored: string | null | undefined): Theme | undefined {
  return chosenTheme(stored);
}

/** The other one, which is what pressing the control asks for. */
export function toggled(current: Theme | undefined, systemIsDark: boolean): Theme {
  const showing = current ?? (systemIsDark ? 'dark' : 'light');
  return showing === 'dark' ? 'light' : 'dark';
}

/** The sRGB channel value of a hex colour, 0 to 1. */
function channels(hex: string): [number, number, number] {
  const value = hex.replace('#', '');
  const full =
    value.length === 3 ? [...value].map((digit) => `${digit}${digit}`).join('') : value.slice(0, 6);
  return [0, 2, 4].map((at) => Number.parseInt(full.slice(at, at + 2), 16) / 255) as [
    number,
    number,
    number,
  ];
}

/** The relative luminance of a colour, as the contrast rule defines it. */
export function luminance(hex: string): number {
  const [red, green, blue] = channels(hex).map((part) =>
    part <= 0.039_28 ? part / 12.92 : ((part + 0.055) / 1.055) ** 2.4,
  ) as [number, number, number];
  return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
}

/** How much two colours differ, as the contrast rule measures it. */
export function contrast(first: string, second: string): number {
  const [lighter, darker] = [luminance(first), luminance(second)].toSorted((a, b) => b - a) as [
    number,
    number,
  ];
  return (lighter + 0.05) / (darker + 0.05);
}

/** What normal text must reach to be readable. */
export const CONTRAST_NORMAL_TEXT = 4.5;

/**
 * The colours one theme of the token file defines. It is parsed
 * rather than imported because the tokens are CSS: the stylesheet is
 * what a browser reads, and this reads the same thing.
 */
export function paletteOf(tokens: string, theme: Theme): Record<string, string> {
  const marker = theme === 'light' ? ':root {' : ":root[data-theme='dark'] {";
  const from = tokens.indexOf(marker);
  if (from === -1) {
    return {};
  }
  const block = tokens.slice(from, tokens.indexOf('}', from));
  const found: Record<string, string> = {};
  for (const match of block.matchAll(/(--[a-z0-9-]+):\s*(#[0-9a-f]{3,8})/gi)) {
    found[match[1] as string] = match[2] as string;
  }
  return found;
}
