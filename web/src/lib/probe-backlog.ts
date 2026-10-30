import * as m from "#lib/paraglide/messages.js";
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
 * API returns null. Null (or an absent field) means "not tracked by the hub",
 * which is different from an empty queue (0) and from a probe that failed to
 * report. Positive bytes mean queued data.
 */
export function probeBacklog(
  probe: Partial<Pick<ProbeView, "queue_bytes" | "oldest_queued_at">>,
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

export interface ProbeBacklogDisplayOptions {
  locale?: "en" | "th";
  formatBytes?: (bytes: number) => string;
  formatTime?: (value: string) => string;
}

/**
 * The two probe-detail cells for the backlog, already localized. An untracked
 * backlog says so in both cells; an empty queue has no oldest item, which is
 * not a reporting failure; a queued backlog without an oldest timestamp is
 * the only case that reads as unreported.
 */
export function probeBacklogDisplay(
  probe: Partial<Pick<ProbeView, "queue_bytes" | "oldest_queued_at">>,
  options: ProbeBacklogDisplayOptions = {},
): { queueBytes: string; oldestQueued: string } {
  const locale = options.locale ? { locale: options.locale } : {};
  const formatBytes =
    options.formatBytes ?? ((n: number) => n.toLocaleString());
  const formatTime =
    options.formatTime ?? ((v: string) => new Date(v).toLocaleString());
  const backlog = probeBacklog(probe);
  switch (backlog.state) {
    case "not_tracked": {
      const label = m.probes_not_tracked({}, locale);
      return { queueBytes: label, oldestQueued: label };
    }
    case "empty":
      return {
        queueBytes: formatBytes(0),
        oldestQueued: m.probes_queue_empty({}, locale),
      };
    case "queued":
      return {
        queueBytes: formatBytes(backlog.bytes ?? 0),
        oldestQueued: backlog.oldestQueuedAt
          ? formatTime(backlog.oldestQueuedAt)
          : m.probes_unreported({}, locale),
      };
  }
}
