import { describe, expect, test } from "bun:test";
import { regionalHistoryStale } from "./regional-history";

const region = {
  observed_at: "2026-11-02T13:39:40Z",
  fresh_until: "2026-11-02T13:42:40Z", // a 3-minute freshness window
};
const beat = (time: string) => ({ time });

describe("regional history staleness (#192)", () => {
  test("a bar whose newest beat is hours behind observed_at is stale", () => {
    // Run A: badge observed 20:39 ICT, newest stored beat 15:44 ICT.
    expect(regionalHistoryStale(region, [beat("2026-11-02T08:44:43Z")])).toBe(
      true,
    );
  });

  test("a bar inside the freshness window is current", () => {
    expect(regionalHistoryStale(region, [beat("2026-11-02T13:38:40Z")])).toBe(
      false,
    );
    expect(regionalHistoryStale(region, [beat("2026-11-02T13:36:40Z")])).toBe(
      false,
    );
  });

  test("only the newest beat counts, whatever the array order", () => {
    expect(
      regionalHistoryStale(region, [
        beat("2026-11-02T08:44:43Z"),
        beat("2026-11-02T13:39:10Z"),
      ]),
    ).toBe(false);
  });

  test("no evidence means no claim", () => {
    expect(regionalHistoryStale(region, [])).toBe(false);
    expect(
      regionalHistoryStale({ observed_at: null, fresh_until: null }, [
        beat("2026-11-02T08:44:43Z"),
      ]),
    ).toBe(false);
    expect(regionalHistoryStale(region, [beat("not a time")])).toBe(false);
  });

  test("without fresh_until a one-minute floor still applies", () => {
    const bare = { observed_at: region.observed_at, fresh_until: null };
    expect(regionalHistoryStale(bare, [beat("2026-11-02T13:39:00Z")])).toBe(
      false,
    );
    expect(regionalHistoryStale(bare, [beat("2026-11-02T13:30:00Z")])).toBe(
      true,
    );
  });
});
