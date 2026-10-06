/**
 * Identifier contract for probe browser events. The wire projection
 * (internal/adapters/ws/regional.go `regionalWirePayload`, published by
 * internal/adapters/http/handlers/regional_browser.go) keeps the two shapes
 * distinct: `probe.status` names the probe with `id`, while `probe.config.status`
 * names it with `probe_id`. Matching both against one key silently drops the
 * other event type — GitHub #61 left the probe detail page showing a stale
 * Connected state after an agent disconnected.
 */
export function probeEventMatches(
  kind: "probe.status" | "probe.config.status",
  payload: unknown,
  probeId: string,
): boolean {
  if (!probeId || typeof payload !== "object" || payload === null) return false;
  const row = payload as Record<string, unknown>;
  const identifier = kind === "probe.status" ? row.id : row.probe_id;
  return typeof identifier === "string" && identifier === probeId;
}
