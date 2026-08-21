import { describe, expect, test } from "bun:test";
import {
  WALLBOARD_CARD_DEFAULT_PX,
  WALLBOARD_CARD_MAX_PX,
  WALLBOARD_CARD_MIN_PX,
  WALLBOARD_CARDS_MIN_PX,
  WALLBOARD_SPLITTER_PX,
  WALLBOARD_STATS_MIN_PX,
  clampWallboardCardPx,
  clampWallboardStatsPx,
  parseWallboardStatsPx,
  wallboardCardScale,
  wallboardStatFontPx,
} from "./wallboard-split";

describe("wallboard split", () => {
  test("parses a stored pixel height", () => {
    expect(parseWallboardStatsPx("128")).toBe(128);
    expect(parseWallboardStatsPx("128.7")).toBe(129);
  });

  test("rejects missing or garbage values", () => {
    expect(parseWallboardStatsPx(undefined)).toBeNull();
    expect(parseWallboardStatsPx(null)).toBeNull();
    expect(parseWallboardStatsPx("")).toBeNull();
    expect(parseWallboardStatsPx("auto")).toBeNull();
    expect(parseWallboardStatsPx("0")).toBeNull();
    expect(parseWallboardStatsPx("-20")).toBeNull();
  });

  test("clamps so cards keep a usable remainder", () => {
    const body = 800;
    const max = body - WALLBOARD_CARDS_MIN_PX - WALLBOARD_SPLITTER_PX;
    expect(clampWallboardStatsPx(40, body)).toBe(WALLBOARD_STATS_MIN_PX);
    expect(clampWallboardStatsPx(900, body)).toBe(max);
    expect(clampWallboardStatsPx(200, body)).toBe(200);
  });

  test("keeps the min stats height when the body is too short", () => {
    expect(clampWallboardStatsPx(400, 100)).toBe(WALLBOARD_STATS_MIN_PX);
  });

  test("scales metric type with pane height", () => {
    expect(wallboardStatFontPx(72)).toBe(20);
    expect(wallboardStatFontPx(200)).toBeGreaterThan(20);
    expect(wallboardStatFontPx(400)).toBe(56);
  });

  test("clamps card min-width", () => {
    expect(clampWallboardCardPx(10)).toBe(WALLBOARD_CARD_MIN_PX);
    expect(clampWallboardCardPx(900)).toBe(WALLBOARD_CARD_MAX_PX);
    expect(clampWallboardCardPx(WALLBOARD_CARD_DEFAULT_PX)).toBe(
      WALLBOARD_CARD_DEFAULT_PX,
    );
  });

  test("card scale is 1 at the default width", () => {
    expect(wallboardCardScale(WALLBOARD_CARD_DEFAULT_PX)).toBe(1);
    expect(wallboardCardScale(WALLBOARD_CARD_DEFAULT_PX * 2)).toBeGreaterThan(
      1,
    );
  });
});
