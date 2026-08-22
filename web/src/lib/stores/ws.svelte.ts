/**
 * Runes-based WebSocket store — single source of real-time state.
 *
 * Usage:
 *   import { realtime } from '$lib/stores/ws.svelte.js';
 *   realtime.connect();
 *   $effect(() => { console.log(realtime.status); });
 */
import type { Status } from "$lib/monitor-types";
import {
  clearMonitorSnapshotCache,
  readMonitorSnapshotCache,
  writeMonitorSnapshotCache,
} from "$lib/monitor-snapshot-cache";
import { isWsAuthFailure } from "$lib/ws-auth-close";
import {
  applyConditionSnapshotToMap,
  conditionKey,
  type ConditionKind,
  type ConditionState,
  type MonitorCondition,
} from "$lib/api/conditions";

// Canonical Status lives in monitor-types.ts (single source of truth). Re-exported
// here so existing `import type { Status } from "$lib/stores/ws.svelte.js"` (if any)
// and local usages keep working.
export type { Status };

export type WsStatus =
  | "connecting"
  | "connected"
  | "disconnected"
  | "reconnecting";

export interface Monitor {
  id: number;
  name: string;
  /** Informational service/team contact; unrelated to the creating user. */
  owner?: string;
  inherit_group_owner?: boolean;
  /** Resolved contact (prefer over owner for display). */
  effective_owner?: string;
  type: string;
  /**
   * Monitor's own display state — mirrors the latest Heartbeat's domain Status
   * (up/down/pending/maintenance) plus one frontend-only addition: "paused"
   * means `active === false`, a state no Heartbeat ever reports.
   */
  status: "up" | "down" | "pending" | "maintenance" | "paused";
  active?: boolean;
  /** Seconds between checks (top-level API/WS field, not inside config). */
  interval?: number;
  /** Check timeout in seconds (top-level API/WS field). */
  timeout?: number;
  target?: string;
  config: Record<string, unknown>;
  description?: string;
  /** Seconds between retries after a failed check. */
  retry_interval?: number;
  /** Retries before marking the monitor DOWN. */
  max_retries?: number;
  /** Re-notify every N consecutive failures (0 = notify once). */
  resend_interval?: number;
  /** Flip UP/DOWN interpretation of the check result. */
  upside_down?: boolean;
  /** HTTP: skip TLS certificate verification — insecure (top-level, not in config). */
  tls_ignore?: boolean;
  /** HTTP: opt into certificate-expiry alerts (30/14/7 day thresholds). */
  cert_expiry_notify?: boolean;
  /** HTTP: accepted status code ranges, e.g. ["200-299", "301"] (top-level, not in config). */
  accepted_statuscodes?: string[];
  /**
   * Manual display order (lower first). Dashboard / folder trees sort by
   * weight, then name. Default from the API is 2000 when omitted on create.
   */
  weight?: number;
  tags?: MonitorTagView[];
  created_at: string;
  updated_at: string;
}

/**
 * A tag as EMBEDDED on a monitor payload, on both the REST list/detail response
 * (internal/adapters/http/handlers/monitor.go MonitorView.Tags) and the WS wire
 * (internal/adapters/ws/wire.go). Always an array — never null.
 *
 * `id`/`name`/`color` are the TAG's; `value` is the per-monitor annotation from
 * the join row (empty string when unset).
 *
 * Distinct from `MonitorTag` in $lib/api/tags.ts, which is the join ROW returned
 * by `GET /monitors/:id/tags` (`{ monitor_id, tag_id, value, tag }`).
 */
export interface MonitorTagView {
  id: number;
  name: string;
  color: string;
  value: string;
}

/**
 * Live heartbeat delivered over the WS `heartbeat` event (internal/adapters/ws/wire.go
 * HeartbeatView): monitor_id, status, time, ping, msg. This is a lighter subset of the
 * full REST Heartbeat record (no id/important) — normalized from the wire payload by
 * normalizeHeartbeat() below. `status` uses the same canonical Status union as the REST
 * API; note the WS wire layer sends "paused" (not "maintenance") for maintenance-window
 * heartbeats, which normalizeHeartbeat maps back to "maintenance" for consistency.
 */
export interface Heartbeat {
  monitor_id: number;
  status: Status;
  time: string;
  ping: number;
  msg?: string;
}

export interface StatsUpdate {
  total: number;
  up: number;
  down: number;
  pending: number;
}

export interface WsEvent {
  type: string;
  payload: unknown;
}

export interface WsDebugEvent {
  type: string;
  monitorId?: number;
  at: string;
}

function appendWsDebugEvent(type: string, monitorId?: number) {
  if (typeof globalThis === "undefined") return;
  const w = globalThis as typeof globalThis & {
    __phoenixWsEventLog?: WsDebugEvent[];
  };
  if (!w.__phoenixWsEventLog) w.__phoenixWsEventLog = [];
  w.__phoenixWsEventLog.push({
    type,
    monitorId,
    at: new Date().toISOString(),
  });
}

/**
 * Map a lowercase wire status string to the canonical Status union. The WS wire
 * layer (internal/adapters/ws/wire.go statusToWire) sends "paused" — not
 * "maintenance" — for maintenance-window heartbeats; map it back here so the
 * live map matches the canonical domain Status used by the REST API.
 */
function normalizeWireStatus(statusRaw: string): Status {
  switch (statusRaw) {
    case "down":
      return "down";
    case "pending":
      return "pending";
    case "maintenance":
    case "paused":
      return "maintenance";
    default:
      return "up";
  }
}

/** Normalize heartbeat wire payloads (Redis / map shapes use MonitorID etc.). */
function normalizeHeartbeat(raw: unknown): Heartbeat | null {
  if (!raw || typeof raw !== "object") return null;
  const o = raw as Record<string, unknown>;
  const monitorId = Number(o.monitor_id ?? o.MonitorID ?? o.monitorId);
  if (!Number.isFinite(monitorId) || monitorId <= 0) return null;
  const statusField = o.status ?? o.Status ?? "";
  const statusRaw = (
    typeof statusField === "string" ? statusField : ""
  ).toLowerCase();
  const status: Status = normalizeWireStatus(statusRaw);
  const time = String(o.time ?? o.Time ?? "");
  const ping = Number(o.ping ?? o.Ping ?? 0);
  const msg = o.msg ?? o.Msg;
  return {
    monitor_id: monitorId,
    status,
    time,
    ping: Number.isFinite(ping) ? ping : 0,
    msg: typeof msg === "string" && msg ? msg : undefined,
  };
}

function normalizeCondition(raw: unknown): MonitorCondition | null {
  if (!raw || typeof raw !== "object") return null;
  const value = raw as Record<string, unknown>;
  const monitorId = Number(
    value.monitor_id ?? value.MonitorID ?? value.monitorId,
  );
  if (!Number.isFinite(monitorId) || monitorId <= 0) return null;
  const kind = String(value.kind ?? value.Kind ?? "") as ConditionKind;
  if (kind !== "session_pool" && kind !== "storage") return null;
  const rawState = String(value.state ?? value.State ?? "").toLowerCase();
  const state: ConditionState = ["ok", "warning", "error", "stale"].includes(
    rawState,
  )
    ? (rawState as ConditionState)
    : "error";
  const nullableNumber = (field: unknown): number | null => {
    if (field == null || field === "") return null;
    const parsed = Number(field);
    return Number.isFinite(parsed) ? parsed : null;
  };
  const nullableString = (field: unknown): string | null =>
    typeof field === "string" && field ? field : null;
  return {
    monitor_id: monitorId,
    kind,
    state,
    used: nullableNumber(value.used ?? value.Used),
    limit: nullableNumber(value.limit ?? value.Limit),
    percent: nullableNumber(value.percent ?? value.Percent),
    threshold: nullableNumber(value.threshold ?? value.Threshold),
    unit: String(value.unit ?? value.Unit ?? ""),
    resource: String(value.resource ?? value.Resource ?? ""),
    scope: String(value.scope ?? value.Scope ?? ""),
    source: String(value.source ?? value.Source ?? ""),
    message: String(value.message ?? value.Message ?? ""),
    observed_at: String(value.observed_at ?? value.ObservedAt ?? ""),
    stale_after: String(value.stale_after ?? value.StaleAfter ?? ""),
    last_success_at: nullableString(
      value.last_success_at ?? value.LastSuccessAt,
    ),
  };
}

function createWsStore() {
  let status = $state<WsStatus>("disconnected");
  let monitors = $state<Monitor[]>([]);
  /** True after the server has sent the initial (possibly empty) monitor list. */
  let hasMonitorSnapshot = $state(false);
  /** Who last filled the snapshot. REST may replace a cache; it must not clobber WS. */
  let snapshotSource: "none" | "cache" | "rest" | "ws" = "none";
  let heartbeats = $state<Map<number, Heartbeat>>(new Map());
  /** Bumped on every heartbeat so UIs can subscribe without relying on monitors identity. */
  let heartbeatSeq = $state(0);
  /** The heartbeat that last arrived over the socket; UIs merge this instead of scanning the map. */
  let lastHeartbeat = $state<Heartbeat | null>(null);
  let conditions = $state<Map<string, MonitorCondition>>(new Map());
  let conditionSeq = $state(0);
  let reconnectAttempt = $state(0);
  let lastError = $state<string | null>(null);
  let stats = $state<StatsUpdate>({ total: 0, up: 0, down: 0, pending: 0 });

  let isConnected = $derived(status === "connected");

  let ws: WebSocket | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  let connectUrl = "";
  let connectGeneration = 0;

  function getWebSocketUrl(): string {
    if (connectUrl) return connectUrl;
    const protocol = location.protocol === "https:" ? "wss" : "ws";
    const base = `${protocol}://${location.host}/ws`;
    const auth = localStorage.getItem("phoenix_jwt");
    connectUrl = auth ? `${base}?token=${encodeURIComponent(auth)}` : base;
    return connectUrl;
  }

  /**
   * Parse an ArrayBuffer message as a typed JSON event.
   * The backend sends binary frames with a 4-byte JSON length prefix.
   */
  function parseArrayBuffer(data: ArrayBuffer): WsEvent | null {
    try {
      const view = new DataView(data);
      if (view.byteLength < 4) return null;
      const jsonLen = view.getUint32(0, true);
      const decoder = new TextDecoder();
      const jsonStr = decoder.decode(data.slice(4, 4 + jsonLen));
      return JSON.parse(jsonStr) as WsEvent;
    } catch {
      return null;
    }
  }

  function handleEvent(event: WsEvent): void {
    switch (event.type) {
      case "monitor.list": {
        const list = Array.isArray(event.payload)
          ? (event.payload as Monitor[])
          : [];
        applyMonitorSnapshot(list);
        for (const m of list) appendWsDebugEvent("monitor.list", m.id);
        break;
      }
      case "monitor.create":
      case "monitor.update": {
        const m = event.payload as Monitor;
        monitors = [...monitors.filter((x) => x.id !== m.id), m];
        appendWsDebugEvent(event.type, m.id);
        break;
      }
      case "monitor.delete": {
        const id = event.payload as number;
        monitors = monitors.filter((x) => x.id !== id);
        heartbeats = new Map([...heartbeats].filter(([k]) => k !== id));
        conditions = new Map(
          [...conditions].filter(
            ([, condition]) => condition.monitor_id !== id,
          ),
        );
        conditionSeq += 1;
        break;
      }
      case "heartbeat": {
        const hb = normalizeHeartbeat(event.payload);
        if (!hb) break;
        appendWsDebugEvent("heartbeat", hb.monitor_id);
        const next = new Map(heartbeats);
        next.set(hb.monitor_id, hb);
        heartbeats = next;
        lastHeartbeat = hb;
        heartbeatSeq += 1;
        // Keep monitor status in sync with the latest check result (skip paused).
        if (hb.status === "up" || hb.status === "down") {
          // Narrow into a local so the union stays "up" | "down" inside the
          // closure below (property narrowing on `hb.status` does not persist
          // across the .map() callback boundary).
          const syncedStatus = hb.status;
          monitors = monitors.map((m) =>
            m.id === hb.monitor_id &&
            m.active !== false &&
            m.status !== "paused"
              ? { ...m, status: syncedStatus }
              : m,
          );
        }
        break;
      }
      case "status.change": {
        const { monitor_id, status: newStatus } = event.payload as {
          monitor_id: number;
          status: string;
        };
        appendWsDebugEvent("status.change", monitor_id);
        monitors = monitors.map((m) =>
          m.id === monitor_id
            ? { ...m, status: newStatus as Monitor["status"] }
            : m,
        );
        break;
      }
      case "condition.update": {
        const condition = normalizeCondition(event.payload);
        if (!condition) break;
        const next = new Map(conditions);
        next.set(conditionKey(condition), condition);
        conditions = next;
        conditionSeq += 1;
        appendWsDebugEvent("condition.update", condition.monitor_id);
        break;
      }
      case "condition.delete": {
        if (!event.payload || typeof event.payload !== "object") break;
        const value = event.payload as Record<string, unknown>;
        const monitorId = Number(
          value.monitor_id ?? value.MonitorID ?? value.monitorId,
        );
        const kind = String(value.kind ?? value.Kind ?? "") as ConditionKind;
        if (
          !Number.isFinite(monitorId) ||
          monitorId <= 0 ||
          (kind !== "session_pool" && kind !== "storage")
        ) {
          break;
        }
        const next = new Map(conditions);
        next.delete(conditionKey({ monitor_id: monitorId, kind }));
        conditions = next;
        conditionSeq += 1;
        appendWsDebugEvent("condition.delete", monitorId);
        break;
      }
      case "stats.update": {
        const s = event.payload as StatsUpdate;
        stats = {
          total: s.total ?? 0,
          up: s.up ?? 0,
          down: s.down ?? 0,
          pending: s.pending ?? 0,
        };
        void import("$lib/utils/favicon.svelte.ts").then(
          ({ updateFaviconBadge }) => updateFaviconBadge(stats.down),
        );
        break;
      }
      default:
        // Unknown events are silently ignored for forward compatibility
        break;
    }
  }

  /** REST payloads omit live status; WS snapshots include it. */
  function withDefaultStatus(monitor: Monitor): Monitor {
    if (monitor.status) return monitor;
    return {
      ...monitor,
      status: monitor.active === false ? "paused" : "pending",
    };
  }

  /**
   * Apply a monitor snapshot. The WS `monitor.list` frame is authoritative and
   * always overwrites. REST hydration is first-paint only relative to WS: it
   * must not clobber a snapshot that already arrived over the socket, but it
   * MAY replace a cached first-paint so a stale sessionStorage list does not
   * stick after monitors were added or deleted.
   */
  function applyMonitorSnapshot(
    list: Monitor[],
    source: "ws" | "rest" | "cache" = "ws",
  ): void {
    if (source === "cache" && snapshotSource !== "none") return;
    if (source === "rest" && snapshotSource === "ws") return;
    monitors = list.map(withDefaultStatus);
    hasMonitorSnapshot = true;
    snapshotSource = source;
    if (source === "cache") return;
    const token =
      typeof localStorage !== "undefined"
        ? localStorage.getItem("phoenix_jwt")
        : null;
    writeMonitorSnapshotCache(token, monitors);
  }

  /**
   * First paint must not depend on a single WS frame. If the hub is slow
   * building monitor.list — or drops it — GET /api/monitors still unblocks
   * the dashboard. Live heartbeats then patch status on the REST rows.
   *
   * Retries a couple of times: a canceled request (navigation, a reconnect
   * that aborted the previous fetch) used to leave the grid on skeleton
   * cards until monitor.list finished — 17s+ when the heartbeat lookup
   * was saturated.
   */
  function snapshotIsFromWs(): boolean {
    return snapshotSource === "ws";
  }

  async function hydrateMonitorsFromRest(): Promise<void> {
    if (snapshotIsFromWs()) return;
    const { monitorsApi } = await import("$lib/api/monitors");
    for (let attempt = 0; attempt < 3; attempt++) {
      if (snapshotIsFromWs()) return;
      try {
        const list = await monitorsApi.list();
        if (snapshotIsFromWs()) return;
        const rows = Array.isArray(list) ? list : [];
        applyMonitorSnapshot(rows, "rest");
        for (const m of rows) appendWsDebugEvent("monitor.list.rest", m.id);
        return;
      } catch {
        if (attempt < 2) {
          await new Promise((resolve) =>
            setTimeout(resolve, 400 * (attempt + 1)),
          );
        }
      }
    }
  }

  function restoreCachedSnapshot(): void {
    if (hasMonitorSnapshot) return;
    const token =
      typeof localStorage !== "undefined"
        ? localStorage.getItem("phoenix_jwt")
        : null;
    const cached = readMonitorSnapshotCache<Monitor>(token);
    if (!cached || cached.length === 0) return;
    applyMonitorSnapshot(cached, "cache");
  }

  function scheduleReconnect(): void {
    status = "reconnecting";
    if (reconnectTimer) clearTimeout(reconnectTimer);
    const delay = Math.min(1000 * 2 ** reconnectAttempt, 30_000);
    reconnectTimer = setTimeout(() => {
      reconnectAttempt++;
      lastError = `Reconnecting (attempt ${reconnectAttempt})`;
      connect();
    }, delay);
  }

  function socketIsLive(): boolean {
    return (
      ws !== null &&
      (ws.readyState === WebSocket.OPEN ||
        ws.readyState === WebSocket.CONNECTING)
    );
  }

  function openSocket(wsUrl: string, generation: number): void {
    if (generation !== connectGeneration) return;
    try {
      ws = new WebSocket(wsUrl);
      ws.binaryType = "arraybuffer";

      ws.onopen = () => {
        if (generation !== connectGeneration) return;
        status = "connected";
        reconnectAttempt = 0;
        lastError = null;
      };

      ws.onmessage = (e: MessageEvent) => {
        let event: WsEvent | null = null;

        if (e.data instanceof ArrayBuffer) {
          event = parseArrayBuffer(e.data);
        } else if (typeof e.data === "string") {
          try {
            event = JSON.parse(e.data) as WsEvent;
          } catch {
            return;
          }
        }

        if (event) handleEvent(event);
      };

      ws.onclose = (e: CloseEvent) => {
        if (generation !== connectGeneration) return;
        status = "disconnected";
        ws = null;
        // 4001–4003 from the hub, plus 1008 from pre-fix servers: the JWT is
        // dead. Reconnecting with it loops 101 → close forever and the
        // dashboard never leaves "pending".
        if (isWsAuthFailure(e.code)) {
          localStorage.removeItem("phoenix_jwt");
          clearMonitorSnapshotCache();
          if (
            typeof window !== "undefined" &&
            !location.pathname.startsWith("/login")
          ) {
            location.href = "/login";
          }
          return;
        }
        if (e.code !== 1000 && e.code !== 1001) {
          // 1000 = normal close, 1001 = going away (page navigation)
          scheduleReconnect();
        }
      };

      ws.onerror = () => {
        lastError = "WebSocket connection error";
      };
    } catch (err) {
      status = "disconnected";
      lastError = err instanceof Error ? err.message : String(err);
      scheduleReconnect();
    }
  }

  function connect(url?: string): void {
    if (url) connectUrl = url;

    const auth = localStorage.getItem("phoenix_jwt");
    if (!auth) {
      disconnect();
      status = "disconnected";
      lastError = "No auth token";
      return; // do not connect without token
    }

    // Retry / layout remount used to call disconnect() here, which closed the
    // socket mid-monitor.list and aborted GET /api/monitors (context canceled).
    if (socketIsLive()) {
      if (snapshotSource !== "ws") void hydrateMonitorsFromRest();
      return;
    }

    disconnect();
    restoreCachedSnapshot();

    const wsUrl = getWebSocketUrl();
    const generation = ++connectGeneration;
    status = "connecting";
    lastError = null;

    // REST list does not wait on latest heartbeats. Give it a head start so
    // the dashboard can paint while the hub is still building monitor.list.
    // Cap the wait so a hung GET cannot delay live events forever.
    void (async () => {
      await Promise.race([
        hydrateMonitorsFromRest(),
        new Promise<void>((resolve) => setTimeout(resolve, 2000)),
      ]);
      openSocket(wsUrl, generation);
    })();
  }

  function disconnect(): void {
    connectGeneration += 1;
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (ws) {
      ws.onclose = null; // prevent reconnect on intentional close
      ws.close(1000);
      ws = null;
    }
    status = "disconnected";
  }

  function send(data: string | Record<string, unknown>): void {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(typeof data === "string" ? data : JSON.stringify(data));
    }
  }

  /** Apply a local monitor patch (e.g. optimistic pause/resume before WS catches up). */
  function patchMonitor(monitor: Monitor): void {
    monitors = [...monitors.filter((x) => x.id !== monitor.id), monitor];
  }

  // Read of conditionSeq. Callers that later applyConditionSnapshot (which
  // increments this) must untrack the read, or a $effect will refetch forever.
  function beginConditionSnapshot(): number {
    return conditionSeq;
  }

  /** Apply a REST snapshot only if no newer live update/delete arrived. */
  function applyConditionSnapshot(
    snapshot: MonitorCondition[] | null,
    startedAt: number,
    monitorId?: number,
  ): boolean {
    const next = applyConditionSnapshotToMap(
      conditions,
      snapshot,
      startedAt,
      conditionSeq,
      monitorId,
    );
    if (!next) return false;
    conditions = next;
    conditionSeq += 1;
    return true;
  }

  return {
    get status() {
      return status;
    },
    get isConnected() {
      return isConnected;
    },
    get monitors() {
      return monitors;
    },
    get hasMonitorSnapshot() {
      return hasMonitorSnapshot;
    },
    get heartbeats() {
      return heartbeats;
    },
    get heartbeatSeq() {
      return heartbeatSeq;
    },
    get lastHeartbeat() {
      return lastHeartbeat;
    },
    get conditions() {
      return conditions;
    },
    get conditionSeq() {
      return conditionSeq;
    },
    get reconnectAttempt() {
      return reconnectAttempt;
    },
    get lastError() {
      return lastError;
    },
    get stats() {
      return stats;
    },
    connect,
    disconnect,
    send,
    patchMonitor,
    beginConditionSnapshot,
    applyConditionSnapshot,
  };
}

/** Singleton WebSocket store — import and use anywhere. */
export const realtime = createWsStore();

/** E2E hook: inspect WS hydration without importing the module in Playwright. */
if (typeof globalThis !== "undefined") {
  (
    globalThis as typeof globalThis & {
      __phoenixRealtime?: typeof realtime;
    }
  ).__phoenixRealtime = realtime;
}
