import * as m from "#lib/paraglide/messages.js";
import type { ProbeView } from "./api/probes";

export type ProbeBacklogState = "not_tracked" | "empty" | "queued";

/** Who stated the backlog: the hub (verified) or the probe (self-reported). */
export type ProbeBacklogSource = "hub" | "probe";

export interface ProbeBacklog {
  state: ProbeBacklogState;
  bytes: number | null;
  oldestQueuedAt: string | null;
  source: ProbeBacklogSource | null;
  /** When the hub accepted the probe's report; null unless source is "probe". */
  reportedAt: string | null;
}

type BacklogFields = Partial<
  Pick<
    ProbeView,
    | "queue_bytes"
    | "oldest_queued_at"
    | "reported_queue_bytes"
    | "reported_oldest_queued_at"
    | "queue_reported_at"
  >
>;

/**
 * Classifies a probe's replay backlog for display (#126, #157).
 *
 * `queue_bytes` is reserved for a hub-verified backlog and is null today. The
 * hub stores what the probe reported in its last accepted health frame as
 * `reported_queue_bytes`; that value is used, labeled as probe-reported,
 * when no hub-verified value exists. Null everywhere means "not tracked by
 * the hub", which is different from an empty queue (0) and from a probe that
 * failed to report. Positive bytes mean queued data.
 */
export function probeBacklog(probe: BacklogFields): ProbeBacklog {
  if (probe.queue_bytes !== null && probe.queue_bytes !== undefined) {
    return {
      state: probe.queue_bytes === 0 ? "empty" : "queued",
      bytes: probe.queue_bytes,
      oldestQueuedAt: probe.oldest_queued_at ?? null,
      source: "hub",
      reportedAt: null,
    };
  }
  const reported = probe.reported_queue_bytes;
  if (reported !== null && reported !== undefined) {
    return {
      state: reported === 0 ? "empty" : "queued",
      bytes: reported,
      oldestQueuedAt: probe.reported_oldest_queued_at ?? null,
      source: "probe",
      reportedAt: probe.queue_reported_at ?? null,
    };
  }
  return {
    state: "not_tracked",
    bytes: null,
    oldestQueuedAt: null,
    source: null,
    reportedAt: null,
  };
}

export interface ProbeBacklogDisplayOptions {
  locale?: "en" | "th";
  formatBytes?: (bytes: number) => string;
  formatTime?: (value: string) => string;
  /** Milliseconds since the epoch used to age a probe report. */
  now?: number;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

/**
 * The probe-detail backlog cells, already localized. An untracked backlog
 * says so in both cells; an empty queue has no oldest item, which is not a
 * reporting failure; a queued backlog without an oldest timestamp is the only
 * case that reads as unreported. `source` labels a probe-reported value with
 * the age of the report, and is null otherwise.
 */
export function probeBacklogDisplay(
  probe: BacklogFields,
  options: ProbeBacklogDisplayOptions = {},
): { queueBytes: string; oldestQueued: string; source: string | null } {
  const locale = options.locale ? { locale: options.locale } : {};
  const formatBytes =
    options.formatBytes ?? ((n: number) => n.toLocaleString());
  const formatTime =
    options.formatTime ?? ((v: string) => new Date(v).toLocaleString());
  const backlog = probeBacklog(probe);
  const source =
    backlog.source === "probe"
      ? m.probes_queue_reported_by_probe(
          {
            age: reportAge(
              backlog.reportedAt,
              options.now ?? Date.now(),
              locale,
              formatTime,
            ),
          },
          locale,
        )
      : null;
  switch (backlog.state) {
    case "not_tracked": {
      const label = m.probes_not_tracked({}, locale);
      return { queueBytes: label, oldestQueued: label, source };
    }
    case "empty":
      return {
        queueBytes: formatBytes(0),
        oldestQueued: m.probes_queue_empty({}, locale),
        source,
      };
    case "queued":
      return {
        queueBytes: formatBytes(backlog.bytes ?? 0),
        oldestQueued: backlog.oldestQueuedAt
          ? formatTime(backlog.oldestQueuedAt)
          : m.probes_unreported({}, locale),
        source,
      };
  }
}

function reportAge(
  reportedAt: string | null,
  now: number,
  locale: { locale?: "en" | "th" },
  formatTime: (value: string) => string,
): string {
  const at = reportedAt ? Date.parse(reportedAt) : Number.NaN;
  if (!reportedAt || Number.isNaN(at)) {
    return m.probes_unreported({}, locale);
  }
  const elapsed = Math.max(0, now - at);
  if (elapsed < MINUTE) return m.time_just_now({}, locale);
  if (elapsed < HOUR) {
    return m.time_minutes_ago({ count: Math.floor(elapsed / MINUTE) }, locale);
  }
  if (elapsed < 48 * HOUR) {
    return m.time_hours_ago({ count: Math.floor(elapsed / HOUR) }, locale);
  }
  return m.probes_queue_reported_at({ time: formatTime(reportedAt) }, locale);
}
