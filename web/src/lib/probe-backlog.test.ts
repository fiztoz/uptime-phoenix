import { describe, expect, it } from "bun:test";
import { probeBacklog, probeBacklogDisplay } from "./probe-backlog";

// #126: the hub does not yet trust or store the probe's self-reported replay
// queue, so null must read as "not tracked by the hub", never as an empty
// queue and never as "the probe did not report".
describe("probeBacklog", () => {
  it("labels a null queue as not tracked, not as empty or unreported", () => {
    expect(probeBacklog({ queue_bytes: null, oldest_queued_at: null })).toEqual(
      {
        state: "not_tracked",
        bytes: null,
        oldestQueuedAt: null,
      },
    );
  });

  it("labels absent fields as not tracked", () => {
    expect(probeBacklog({}).state).toBe("not_tracked");
  });

  it("keeps a reported zero as an empty queue", () => {
    expect(probeBacklog({ queue_bytes: 0, oldest_queued_at: null }).state).toBe(
      "empty",
    );
  });

  it("keeps a reported backlog with its age", () => {
    expect(
      probeBacklog({
        queue_bytes: 4096,
        oldest_queued_at: "2026-10-28T09:35:00Z",
      }),
    ).toEqual({
      state: "queued",
      bytes: 4096,
      oldestQueuedAt: "2026-10-28T09:35:00Z",
    });
  });
});

// The rendered probe-detail cells, with the real compiled messages in both
// shipped locales.
describe("probeBacklogDisplay", () => {
  const fmt = {
    formatBytes: (n: number) => `${n} B`,
    formatTime: (v: string) => `at ${v}`,
  };

  it.each([
    ["en", "Not tracked by the hub yet"],
    ["th", "ฮับยังไม่ได้ติดตามค่านี้"],
  ] as const)(
    "renders null and absent as not tracked (%s)",
    (locale, label) => {
      for (const probe of [{ queue_bytes: null, oldest_queued_at: null }, {}]) {
        expect(probeBacklogDisplay(probe, { ...fmt, locale })).toEqual({
          queueBytes: label,
          oldestQueued: label,
        });
      }
    },
  );

  it.each([
    ["en", "Nothing queued"],
    ["th", "ไม่มีข้อมูลค้างในคิว"],
  ] as const)(
    "renders zero bytes with no oldest item as an empty queue (%s)",
    (locale, label) => {
      const cells = probeBacklogDisplay(
        { queue_bytes: 0, oldest_queued_at: null },
        { ...fmt, locale },
      );
      expect(cells).toEqual({ queueBytes: "0 B", oldestQueued: label });
      expect(cells.oldestQueued).not.toBe(
        probeBacklogDisplay({ queue_bytes: 1 }, { ...fmt, locale })
          .oldestQueued,
      );
    },
  );

  it("renders queued bytes with the oldest timestamp", () => {
    expect(
      probeBacklogDisplay(
        { queue_bytes: 4096, oldest_queued_at: "2026-10-28T09:35:00Z" },
        { ...fmt, locale: "en" },
      ),
    ).toEqual({
      queueBytes: "4096 B",
      oldestQueued: "at 2026-10-28T09:35:00Z",
    });
  });

  it("only a queued backlog without its age reads as unreported", () => {
    expect(
      probeBacklogDisplay(
        { queue_bytes: 10, oldest_queued_at: null },
        {
          ...fmt,
          locale: "en",
        },
      ).oldestQueued,
    ).toBe("Unreported");
  });
});
