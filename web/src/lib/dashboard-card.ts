export type DashboardCardBody = "response" | "signals";

export const DASHBOARD_CARD_BODIES = ["response", "signals"] as const;
export const DASHBOARD_CARD_STORAGE_KEY = "phoenix_dashboard_card";

/** Parse a stored or form value. Unknown input falls back to response. */
export function parseDashboardCardBody(
  raw: string | null | undefined,
): DashboardCardBody {
  return raw === "signals" ? "signals" : "response";
}

export function readDashboardCardBody(): DashboardCardBody {
  if (typeof localStorage === "undefined") return "response";
  return parseDashboardCardBody(
    localStorage.getItem(DASHBOARD_CARD_STORAGE_KEY),
  );
}

export function writeDashboardCardBody(body: DashboardCardBody): void {
  if (typeof localStorage === "undefined") return;
  localStorage.setItem(DASHBOARD_CARD_STORAGE_KEY, body);
}

/** Capacity meters replace the sparkline only when that view is selected and signals exist. */
export function cardUsesSignals(
  body: DashboardCardBody,
  conditionCount: number,
): boolean {
  return body === "signals" && conditionCount > 0;
}
