import { api } from "./client";

export type ConditionState = "ok" | "warning" | "error" | "stale";
export type ConditionKind = "session_pool" | "storage";

export interface MonitorCondition {
  monitor_id: number;
  kind: ConditionKind;
  state: ConditionState;
  used: number | null;
  limit: number | null;
  percent: number | null;
  threshold: number | null;
  unit: string;
  resource: string;
  scope: string;
  source: string;
  message: string;
  observed_at: string;
  stale_after: string;
  last_success_at: string | null;
}

export function displayedConditionState(
  condition: MonitorCondition,
  now = Date.now(),
): ConditionState {
  const staleAt = Date.parse(condition.stale_after);
  if (Number.isFinite(staleAt) && now >= staleAt) return "stale";
  return condition.state;
}

export function conditionNeedsAttention(
  condition: MonitorCondition,
  now = Date.now(),
): boolean {
  return displayedConditionState(condition, now) !== "ok";
}

export function conditionDrivesDashboardAttention(
  condition: MonitorCondition,
  monitor: { active?: boolean; status?: string } | undefined,
  now = Date.now(),
): boolean {
  if (!monitor || monitor.active === false) return false;
  if (monitor.status === "paused" || monitor.status === "maintenance") {
    return false;
  }
  return conditionNeedsAttention(condition, now);
}

export function conditionKey(
  condition: Pick<MonitorCondition, "monitor_id" | "kind">,
): string {
  return `${condition.monitor_id}:${condition.kind}`;
}

export function applyConditionSnapshotToMap(
  current: Map<string, MonitorCondition>,
  snapshot: MonitorCondition[] | null,
  startedAt: number,
  currentSeq: number,
  monitorId?: number,
): Map<string, MonitorCondition> | null {
  if (snapshot === null || startedAt !== currentSeq) return null;
  if (monitorId == null) {
    return new Map(
      snapshot.map((condition) => [conditionKey(condition), condition]),
    );
  }
  const next = new Map(
    [...current].filter(([, condition]) => condition.monitor_id !== monitorId),
  );
  for (const condition of snapshot) {
    next.set(conditionKey(condition), condition);
  }
  return next;
}

export const conditionsApi = {
  async list(monitorId?: number): Promise<MonitorCondition[]> {
    return api.get<MonitorCondition[]>(
      "/monitor-conditions",
      monitorId == null ? undefined : { monitor_id: monitorId },
    );
  },
};
