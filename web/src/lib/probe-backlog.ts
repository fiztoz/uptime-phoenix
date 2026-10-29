import type { ProbeView } from "./api/probes";

export type ProbeBacklogState = "not_tracked" | "empty" | "queued";

export interface ProbeBacklog {
  state: ProbeBacklogState;
  bytes: number | null;
  oldestQueuedAt: string | null;
}

/**
 * Classifies a probe's replay backlog for display (#126).
 *
 * The hub does not persist the probe's self-reported `queue_bytes` yet, so the
 * API returns null. Null means "not tracked by the hub", which is different
 * from an empty queue (0) and from a probe that failed to report.
 */
export function probeBacklog(
  probe: Pick<ProbeView, "queue_bytes" | "oldest_queued_at">,
): ProbeBacklog {
  if (probe.queue_bytes === null || probe.queue_bytes === undefined) {
    return { state: "not_tracked", bytes: null, oldestQueuedAt: null };
  }
  return {
    state: probe.queue_bytes === 0 ? "empty" : "queued",
    bytes: probe.queue_bytes,
    oldestQueuedAt: probe.oldest_queued_at ?? null,
  };
}
