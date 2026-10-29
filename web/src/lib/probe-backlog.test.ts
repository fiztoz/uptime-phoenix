import { describe, expect, it } from "bun:test";
import { probeBacklog } from "./probe-backlog";

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
