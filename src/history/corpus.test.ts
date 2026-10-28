import { describe, expect, it } from 'vitest';
import { REQUIRED_SHAPES, type Shape, corpus } from './corpus.js';

describe('the generated corpus', () => {
  it('holds at least two hundred cases', () => {
    expect(corpus().length).toBeGreaterThanOrEqual(200);
  });

  it.each(REQUIRED_SHAPES)('covers %s', (shape: Shape) => {
    expect(corpus().filter((one) => one.shapes.includes(shape)).length).toBeGreaterThan(0);
  });

  it('holds a stack of at least twenty patches', () => {
    const deep = corpus().filter((one) => one.branch.length >= 20);

    expect(deep.length).toBeGreaterThan(0);
    expect(deep.some((one) => one.shapes.includes('deep-stack'))).toBe(true);
  });

  it('covers both operations', () => {
    const operations = new Set(corpus().map((one) => one.operation));

    expect([...operations].toSorted()).toEqual(['merge', 'rebase']);
  });

  it('holds cases that conflict and cases that do not', () => {
    const all = corpus();

    expect(all.filter((one) => one.shapes.includes('conflicting')).length).toBeGreaterThan(10);
    expect(all.filter((one) => one.shapes.includes('clean')).length).toBeGreaterThan(10);
  });

  it('names every case distinctly, so a failure says which one', () => {
    const names = corpus().map((one) => one.name);

    expect(new Set(names).size).toBe(names.length);
  });

  it('generates the same corpus every time, so a failure can be looked at again', () => {
    expect(JSON.stringify(corpus())).toBe(JSON.stringify(corpus()));
  });

  it('gives every case a root and at least one commit on each side', () => {
    for (const one of corpus()) {
      expect(Object.keys(one.root).length).toBeGreaterThan(0);
      expect(one.base.length).toBeGreaterThan(0);
      expect(one.branch.length).toBeGreaterThan(0);
    }
  });
});
