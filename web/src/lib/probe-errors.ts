import * as m from "$lib/paraglide/messages.js";
import { errorCode, errorMessage } from "$lib/api/probes";

/**
 * Maps a failed probe or regional-assignment write to a localized,
 * operator-facing message.
 *
 * Three codes carry a meaning the generic fallback would erase:
 *
 * - `stale_revision` — someone else changed this configuration; reload first.
 * - `worker_fleet_unaware` — the mixed-version guard (verification matrix T34).
 *   The hub refused to make a monitor remote because at least one live worker
 *   has not attested that it honors probe assignments, so handing it over would
 *   let that worker keep checking it locally and fight the regional evidence.
 *   This is a rollout state, not a mistake in the form: retrying the same save
 *   cannot succeed until the upgrade finishes, and the server message names
 *   worker IDs that belong in logs rather than in the UI.
 * - `worker_readiness_unavailable` — the same guard failing closed because the
 *   readiness table could not be read. Also transient, also not a form error.
 *
 * Anything else falls through to the server's own message so a new backend code
 * still surfaces something truthful instead of a blanket "save failed".
 */
export function probeWriteErrorMessage(
  error: unknown,
  fallback: string,
): string {
  switch (errorCode(error)) {
    case "stale_revision":
      return m.probes_stale();
    case "worker_fleet_unaware":
      return m.probes_fleet_unaware();
    case "worker_readiness_unavailable":
      return m.probes_readiness_unavailable();
    default:
      return errorMessage(error, fallback);
  }
}
