// The Regional health row reads its badge from /health (current state) and its
// bar from the history endpoint. They come from different pipelines: current
// state can stay fresh while stored history stops (#192). The bar is stale
// when its newest beat is older than the row's observed_at by more than the
// freshness window the hub itself uses for that badge (fresh_until -
// observed_at), with a one-minute floor.

const MIN_WINDOW_MS = 60_000;

/** Whether a region's history bar lags its current observation. */
export function regionalHistoryStale(
  region: { observed_at: string | null; fresh_until: string | null },
  beats: ReadonlyArray<{ time: string }>,
): boolean {
  const observed = region.observed_at ? Date.parse(region.observed_at) : NaN;
  if (!Number.isFinite(observed)) return false;
  let newest = -Infinity;
  for (const beat of beats) {
    const at = Date.parse(beat.time);
    if (Number.isFinite(at) && at > newest) newest = at;
  }
  if (!Number.isFinite(newest)) return false;
  const freshUntil = region.fresh_until ? Date.parse(region.fresh_until) : NaN;
  const window = Number.isFinite(freshUntil)
    ? Math.max(freshUntil - observed, MIN_WINDOW_MS)
    : MIN_WINDOW_MS;
  return observed - newest > window;
}
