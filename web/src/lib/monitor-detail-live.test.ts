/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { mergeLiveStatusHistory } from "./monitor-detail-live";
import type { Heartbeat } from "./monitor-types";

const row = (over: Partial<Heartbeat>): Heartbeat => ({
  id: 1,
  monitor_id: 1,
  status: "up",
  ping: 0,
  message: "",
  time: "2026-11-02T04:33:08Z",
  important: true,
  ...over,
});

describe("mergeLiveStatusHistory", () => {
  // GitHub #197: every same-status live beat overwrote the leading row's time,
  // so "Up since 11:33:08" read as the latest check time.
  test("a same-status beat keeps the transition row's time", () => {
    const transition = row({
      id: 9,
      time: "2026-11-02T04:33:08Z",
      message: "",
    });
    const live = row({ id: -1, time: "2026-11-02T14:07:37Z", ping: 31 });
    const next = mergeLiveStatusHistory([transition], live, "up");
    expect(next).toHaveLength(1);
    expect(next[0].time).toBe("2026-11-02T04:33:08Z");
  });

  test("a same-status beat keeps the transition row's message", () => {
    // A DOWN row's message is why the outage started; the next failing
    // check's message must not replace it (the server row would bring it
    // back on the next refetch, and the two would keep fighting).
    const transition = row({
      id: 9,
      status: "down",
      time: "2026-11-02T04:33:08Z",
      message: "connection refused",
    });
    const live = row({
      id: -1,
      status: "down",
      time: "2026-11-02T04:35:08Z",
      message: "i/o timeout",
    });
    const next = mergeLiveStatusHistory([transition], live, "down");
    expect(next[0]).toEqual(transition);
  });

  test("a status change prepends a row with the beat's time", () => {
    const transition = row({ id: 9 });
    const live = row({
      id: -1,
      status: "down",
      time: "2026-11-02T05:00:00Z",
    });
    const next = mergeLiveStatusHistory([transition], live, "up");
    expect(next.map((h) => h.status)).toEqual(["down", "up"]);
    expect(next[0].time).toBe("2026-11-02T05:00:00Z");
    expect(next[1].time).toBe("2026-11-02T04:33:08Z");
  });

  test("a duplicate transition is not prepended twice", () => {
    const live = row({ id: -1, status: "down", time: "2026-11-02T05:00:00Z" });
    const history = [live, row({ id: 9 })];
    expect(mergeLiveStatusHistory(history, live, "up")).toBe(history);
  });

  test("an empty history is seeded from the live beat", () => {
    const live = row({ id: -1 });
    expect(mergeLiveStatusHistory([], live, null)).toEqual([live]);
  });
});
