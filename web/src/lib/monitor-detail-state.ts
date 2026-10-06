/**
 * Pure helpers for monitor detail display state — API fallback, status derivation,
 * and post-clear live heartbeat acceptance. Unit-tested in isolation.
 */
import type { Monitor } from "$lib/stores/ws.svelte.ts";
import type { Status } from "$lib/monitor-types";

export type MonitorStatus = Monitor["status"];

export interface LiveHeartbeat {
  status: string;
  time: string;
  ping: number;
  msg?: string;
  /** Policy status for a remotely assigned monitor (WS wire `overall_status`). */
  overall_status?: Status;
}

/**
 * A Heartbeat's status uses the canonical domain Status union (up/down/pending/
 * maintenance) — distinct from Monitor["status"] (up/down/pending/maintenance/
 * paused). Do not conflate the two: a Heartbeat never reports "paused", and a
 * Monitor's "paused" state is a separate frontend concept (`active === false`),
 * not a heartbeat status.
 */
export interface HistoryHeartbeat {
  status: Status;
  time: string;
}

export interface StatsHint {
  current_ping_ms?: number | null;
}

/**
 * Status (Heartbeat) is a structural subset of MonitorStatus — every domain
 * status, including "maintenance", now has its own real Monitor-status
 * counterpart and passes through unchanged. Only "paused" (Monitor-only,
 * derived from `active === false`) has no Heartbeat equivalent, so callers
 * still go through this seam rather than assigning `Status` to `MonitorStatus`
 * directly at each call site.
 */
function toMonitorStatusHint(
  status: Status | undefined,
): MonitorStatus | undefined {
  return status;
}

/** Timestamp in ms, or null when the value cannot be parsed. */
function parsedTime(value: string | undefined): number | null {
  if (value == null) return null;
  const ms = new Date(value).getTime();
  return Number.isNaN(ms) ? null : ms;
}

/**
 * Prefer the newer of the history row and the live heartbeat; the versioned
 * live stream wins same-second ties (heartbeat timestamps are second-precision
 * on MariaDB, so ties are common). An older history row therefore can never
 * regress the displayed status — GitHub #57.
 */
function newerStatusHint(
  historyStatus: MonitorStatus | undefined,
  historyTime: string | undefined,
  liveStatus: MonitorStatus | undefined,
  liveTime: string | undefined,
): MonitorStatus | undefined {
  if (liveStatus == null) return historyStatus;
  if (historyStatus == null) return liveStatus;
  const historyMs = parsedTime(historyTime);
  const liveMs = parsedTime(liveTime);
  if (historyMs == null) return liveStatus;
  if (liveMs == null) return historyStatus;
  return liveMs >= historyMs ? liveStatus : historyStatus;
}

/**
 * Status hint from live WS data (newest first), then history, then stats ping
 * presence. A live heartbeat contributes its policy status (`overall_status`)
 * when present — its raw `status` is only the local measurement and must not
 * surface as overall health for a mixed-source monitor.
 */
export function deriveStatusHint(
  latestHistory: HistoryHeartbeat | null | undefined,
  liveHb: LiveHeartbeat | null | undefined,
  stats: StatsHint | null | undefined,
): MonitorStatus | null {
  const liveStatus = liveHb
    ? toMonitorStatusHint(
        liveHb.overall_status ?? (liveHb.status as Status | undefined),
      )
    : undefined;
  return (
    newerStatusHint(
      toMonitorStatusHint(latestHistory?.status),
      latestHistory?.time,
      liveStatus,
      liveHb?.time,
    ) ??
    (stats?.current_ping_ms != null && stats.current_ping_ms > 0 ? "up" : null)
  );
}

/** Map API monitor payload to a display monitor with derived status. */
export function monitorFromApi(
  raw: Monitor,
  statusHint: MonitorStatus | null = null,
): Monitor {
  const status: MonitorStatus =
    raw.active === false ? "paused" : (statusHint ?? raw.status ?? "pending");
  return { ...raw, status };
}

/**
 * Prefer realtime WS entry; fall back to API fetch.
 * When both exist, merge so top-level scheduling fields from the API
 * survive if a partial WS payload omits them.
 */
export function resolveDisplayedMonitor(
  live: Monitor | null | undefined,
  fetched: Monitor | null | undefined,
): Monitor | null {
  if (live && fetched) {
    return {
      ...fetched,
      ...live,
      interval: live.interval || fetched.interval,
      timeout: live.timeout || fetched.timeout,
      // Keep status/active from live (real-time), config from live if present.
      config: live.config ?? fetched.config,
    };
  }
  return live ?? fetched ?? null;
}

/**
 * The monitor the detail page displays: live WS row + API fetch + optional
 * status hint. The live row carries the current, versioned overall projection,
 * so the hint only fills the API-only case — letting older history overwrite
 * live health kept the badge UP through outages (GitHub #57).
 */
export function displayedMonitor(
  live: Monitor | null | undefined,
  fetched: Monitor | null | undefined,
  hint: MonitorStatus | null,
): Monitor | null {
  const base = resolveDisplayedMonitor(live, fetched);
  if (!base || base.active === false) return base;
  if (live) return base;
  return hint && hint !== base.status ? { ...base, status: hint } : base;
}

/** Display fields a live WS heartbeat contributes to history/timeline rows. */
export interface LiveDisplayRow {
  status: Status;
  ping: number;
  message: string;
  time: string;
  scope: "local" | "overall";
  latency_available: boolean;
}

/**
 * The row a live WS heartbeat contributes to the detail page's history and
 * timeline streams. A remote assignment reports the policy result in
 * `overall_status`; its raw `status` is one local measurement and must never
 * drive the overall streams — an `any_down` monitor stays DOWN while local
 * checks are UP. Overall rows follow the section-7.2 shape: zero ping,
 * `latency_available: false` (GitHub #57).
 */
export function liveDisplayRow(live: LiveHeartbeat): LiveDisplayRow {
  const overall = live.overall_status;
  if (overall != null) {
    return {
      status: overall,
      ping: 0,
      message: live.msg ?? "",
      time: live.time,
      scope: "overall",
      latency_available: false,
    };
  }
  return {
    status: live.status as Status,
    ping: live.ping,
    message: live.msg ?? "",
    time: live.time,
    scope: "local",
    latency_available: true,
  };
}

export interface AcceptLiveHeartbeatInput {
  /** ISO timestamp when history was cleared; heartbeats at or before this are ignored. */
  clearedAt: string | null;
  hbTime: string;
  lastProcessedTime: string | null;
}

/** Latest heartbeat timestamp from observability rows (ISO), or null when empty. */
export function latestObservedTime(
  rows: Array<{ time: string }>,
): string | null {
  let latest: string | null = null;
  let latestMs = Number.NEGATIVE_INFINITY;
  for (const row of rows) {
    const ms = new Date(row.time).getTime();
    if (!Number.isNaN(ms) && ms > latestMs) {
      latestMs = ms;
      latest = row.time;
    }
  }
  return latest;
}

/** Keep only heartbeats recorded after history was cleared. */
export function heartbeatsAfterClear<T extends { time: string }>(
  rows: T[],
  clearedAt: string | null,
): T[] {
  if (!clearedAt) return rows;
  const clearedMs = new Date(clearedAt).getTime();
  if (Number.isNaN(clearedMs)) return rows;
  return rows.filter((row) => {
    const rowMs = new Date(row.time).getTime();
    return !Number.isNaN(rowMs) && rowMs > clearedMs;
  });
}

/** Stop post-clear API polling once observability data is back. */
export function shouldStopPostClearPolling(
  timelineCount: number,
  historyCount: number,
): boolean {
  return timelineCount > 0 || historyCount > 0;
}

/** Whether monitor detail is driven by API fetch vs WS store hydration. */
export function monitorDataSource(
  monitorId: number,
  liveMonitors: Array<{ id: number }>,
  fetched: Monitor | null | undefined,
): "loading" | "api" | "ws" {
  if (liveMonitors.some((m) => m.id === monitorId)) return "ws";
  if (fetched) return "api";
  return "loading";
}

/** Whether a WS heartbeat should merge into chart/history/timeline after a clear. */
export function acceptLiveHeartbeat(input: AcceptLiveHeartbeatInput): boolean {
  const { clearedAt, hbTime, lastProcessedTime } = input;
  if (lastProcessedTime === hbTime) return false;
  if (clearedAt) {
    const clearedMs = new Date(clearedAt).getTime();
    const hbMs = new Date(hbTime).getTime();
    if (!Number.isNaN(clearedMs) && !Number.isNaN(hbMs) && hbMs <= clearedMs) {
      return false;
    }
  }
  return true;
}
