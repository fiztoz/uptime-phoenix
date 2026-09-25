/** How a projection version compares with the newest one already applied. */
export type ProjectionDecision = "unversioned" | "current" | "newer" | "stale";

/**
 * Local checks omit a version and stay applicable. A newer version advances.
 * An older one is ignored so a replay cannot move the badge backwards.
 */
export function decideProjectionVersion(
  previous: number | undefined,
  version: number,
): ProjectionDecision {
  if (!Number.isFinite(version) || version <= 0) return "unversioned";
  if (previous == null || version > previous) return "newer";
  if (version === previous) return "current";
  return "stale";
}

/** Coalesce navigation-cache clears onto one scheduled turn. */
export function createProjectionInvalidator(
  clear: () => void,
  schedule: (run: () => void) => void = (run) => {
    if (typeof requestAnimationFrame === "function") requestAnimationFrame(run);
    else run();
  },
): () => void {
  let queued = false;
  return () => {
    if (queued) return;
    queued = true;
    schedule(() => {
      queued = false;
      clear();
    });
  };
}
