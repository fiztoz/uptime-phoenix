/**
 * Fire `onVisible` once the node is near the viewport, then disconnect.
 * Dashboard sparkline fetches use this so 60+ cards do not open 60 GETs
 * on first paint — only the cards about to be seen.
 */
export function whenVisible(
  node: HTMLElement,
  onVisible?: () => void,
): { destroy(): void; update(next?: () => void): void } {
  let callback = onVisible;
  let fired = false;
  let observer: IntersectionObserver | null = null;

  function disconnect() {
    observer?.disconnect();
    observer = null;
  }

  function observe() {
    disconnect();
    if (fired || !callback || typeof IntersectionObserver === "undefined") {
      if (!fired && callback && typeof IntersectionObserver === "undefined") {
        callback();
        fired = true;
      }
      return;
    }
    observer = new IntersectionObserver(
      (entries) => {
        if (fired || !callback) return;
        if (entries.some((entry) => entry.isIntersecting)) {
          fired = true;
          callback();
          disconnect();
        }
      },
      { root: null, rootMargin: "320px 0px", threshold: 0.01 },
    );
    observer.observe(node);
  }

  observe();

  return {
    update(next?: () => void) {
      callback = next;
      if (!fired) observe();
    },
    destroy() {
      disconnect();
    },
  };
}
