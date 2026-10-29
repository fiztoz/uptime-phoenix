/// <reference types="bun-types" />
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import { plotClipSize } from "./chart";

describe("plotClipSize (#132)", () => {
  test("a container narrower than the chart padding gives a zero-size clip, not a negative one", () => {
    // 6 px container - (48 left + 16 right) padding = -58, as logged on the status page.
    expect(plotClipSize(6 - 64, -10)).toEqual({ width: 0, height: 0 });
  });

  test("normal sizes pass through", () => {
    expect(plotClipSize(300, 120)).toEqual({ width: 300, height: 120 });
  });

  test("non-finite sizes become zero", () => {
    expect(plotClipSize(Number.NaN, Number.POSITIVE_INFINITY)).toEqual({
      width: 0,
      height: 0,
    });
  });

  test("PlotClip sizes its clip rect through plotClipSize", () => {
    const src = readFileSync(
      join(import.meta.dir, "../components/charts/PlotClip.svelte"),
      "utf8",
    );
    expect(src).toContain("plotClipSize(");
    expect(src).not.toMatch(/width=\{k\.width\}/);
  });
});
