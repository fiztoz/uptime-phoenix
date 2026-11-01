// Message key naming a monitor's saved alert delivery mode (#175). An absent
// legacy mode is regional, as the assignment API documents.
export type AlertDeliveryMessageKey =
  | "probes_delivery_regional"
  | "probes_delivery_aggregate"
  | "probes_delivery_both";

export function alertDeliveryMessageKey(
  mode: string | undefined,
): AlertDeliveryMessageKey {
  switch (mode) {
    case "aggregate":
      return "probes_delivery_aggregate";
    case "both":
      return "probes_delivery_both";
    default:
      return "probes_delivery_regional";
  }
}
