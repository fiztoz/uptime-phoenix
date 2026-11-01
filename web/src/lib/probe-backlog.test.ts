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
        source: null,
        reportedAt: null,
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
      source: "hub",
      reportedAt: null,
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
          source: null,
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
      expect(cells).toEqual({
        queueBytes: "0 B",
        oldestQueued: label,
        source: null,
      });
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
      source: null,
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

// #157: the hub now stores what the probe reported in its last accepted health
// frame. It is shown as probe-reported with the report's age; the hub-verified
// queue_bytes stays null and never-reported still reads as not tracked.
describe("probe-reported queue", () => {
  const now = Date.parse("2026-11-01T07:00:00Z");
  const fmt = {
    formatBytes: (n: number) => `${n} B`,
    formatTime: (v: string) => `T(${v})`,
    now,
  };
  const reported = {
    queue_bytes: null,
    oldest_queued_at: null,
    reported_queue_bytes: 4096,
    reported_oldest_queued_at: "2026-11-01T06:50:00.123456Z",
    queue_reported_at: "2026-11-01T06:55:00Z",
  };

  it("classifies a probe report as a probe-sourced backlog", () => {
    expect(probeBacklog(reported)).toEqual({
      state: "queued",
      bytes: 4096,
      oldestQueuedAt: "2026-11-01T06:50:00.123456Z",
      source: "probe",
      reportedAt: "2026-11-01T06:55:00Z",
    });
    expect(
      probeBacklog({
        ...reported,
        reported_queue_bytes: 0,
        reported_oldest_queued_at: null,
      }).state,
    ).toBe("empty");
  });

  it("a hub-verified value wins over the probe's report", () => {
    expect(probeBacklog({ ...reported, queue_bytes: 1 }).source).toBe("hub");
  });

  it("never-reported fields keep not tracked and no source", () => {
    const backlog = probeBacklog({
      queue_bytes: null,
      oldest_queued_at: null,
      reported_queue_bytes: null,
      reported_oldest_queued_at: null,
      queue_reported_at: null,
    });
    expect(backlog.state).toBe("not_tracked");
    expect(backlog.source).toBeNull();
    expect(
      probeBacklogDisplay(
        { reported_queue_bytes: null, queue_reported_at: null },
        { ...fmt, locale: "en" },
      ).source,
    ).toBeNull();
  });

  it.each([
    ["en", "Reported by probe 5 minutes ago", "Nothing queued"],
    ["th", "รายงานโดยโพรบ 5 นาทีที่แล้ว", "ไม่มีข้อมูลค้างในคิว"],
  ] as const)("labels the report and its age (%s)", (locale, source, empty) => {
    expect(probeBacklogDisplay(reported, { ...fmt, locale })).toEqual({
      queueBytes: "4096 B",
      oldestQueued: "T(2026-11-01T06:50:00.123456Z)",
      source,
    });
    expect(
      probeBacklogDisplay(
        {
          ...reported,
          reported_queue_bytes: 0,
          reported_oldest_queued_at: null,
        },
        { ...fmt, locale },
      ),
    ).toEqual({ queueBytes: "0 B", oldestQueued: empty, source });
  });

  it.each([
    ["2026-11-01T06:59:40Z", "Reported by probe just now"],
    ["2026-11-01T04:00:00Z", "Reported by probe 3 hours ago"],
    ["2026-10-29T07:00:00Z", "Reported by probe at T(2026-10-29T07:00:00Z)"],
  ])("ages a report from %s", (at, label) => {
    expect(
      probeBacklogDisplay(
        { ...reported, queue_reported_at: at },
        { ...fmt, locale: "en" },
      ).source,
    ).toBe(label);
  });
});
