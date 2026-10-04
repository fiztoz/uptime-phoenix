import type { Heartbeat, Status } from "$lib/monitor-types";

/** Choose an unambiguous measured source; multiple regions keep overall policy. */
export function defaultCheckProbe(
  assignments: Array<{ probe_id: string }>,
): string | null {
  return assignments.length === 1 && assignments[0].probe_id !== "local"
    ? assignments[0].probe_id
    : null;
}

/** Merge checks from one source, retaining newer live rows during a snapshot. */
export function mergeRecentChecks(
  snapshot: Heartbeat[],
  live: Heartbeat[],
  limit = 60,
): Heartbeat[] {
  const rows = new Map(snapshot.map((beat) => [beat.id, beat]));
  for (const beat of live) rows.set(beat.id, beat);
  return [...rows.values()]
    .sort((a, b) => Date.parse(a.time) - Date.parse(b.time) || a.id - b.id)
    .slice(-limit);
}

/** Accept only real regional heartbeat events for the currently selected source. */
export function regionalCheckEvent(
  payload: unknown,
  monitorId: number,
  probeId: string | null,
): Heartbeat | null {
  if (!probeId || typeof payload !== "object" || payload === null) return null;
  const row = payload as Record<string, unknown>;
  const statuses: Status[] = [
    "up",
    "down",
    "pending",
    "maintenance",
    "unknown",
  ];
  if (
    row.monitor_id !== monitorId ||
    row.probe_id !== probeId ||
    typeof row.id !== "number" ||
    !Number.isSafeInteger(row.id) ||
    row.id <= 0 ||
    typeof row.time !== "string" ||
    !Number.isFinite(Date.parse(row.time)) ||
    !statuses.includes(row.status as Status)
  )
    return null;
  return {
    id: row.id,
    monitor_id: monitorId,
    status: row.status as Status,
    time: row.time,
    ping:
      typeof row.ping === "number" && Number.isFinite(row.ping) ? row.ping : 0,
    message: typeof row.message === "string" ? row.message : "",
    important: row.important === true,
    scope: "regional",
    latency_available: true,
  };
}
