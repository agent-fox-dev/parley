/**
 * Measuring the tokens' colours.
 *
 * This is an instrument rather than a part of the interface: the
 * contrast rule is a ratio between two colours, the colours are in
 * the token file, and the suites compute the ratios instead of
 * taking a screenshot. Nothing a person sees comes through here.
 */
export type Theme = 'dark' | 'light';

/** What normal text must reach to be readable. */
export const CONTRAST_NORMAL_TEXT = 4.5;

/** The sRGB channels of a hex colour, 0 to 1. */
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

/** The colours one theme of the token file defines. */
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
