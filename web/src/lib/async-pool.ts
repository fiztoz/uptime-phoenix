/**
 * Cap in-flight async work so a page with many rows cannot open one request
 * per row at once. The dashboard used to fire a heartbeat-history GET for every
 * monitor the instant the snapshot arrived; at tens of monitors that saturates
 * the API and cancels the WebSocket monitor.list still being built.
 */

export interface KeyedPool<K> {
  enqueue(keys: readonly K[]): void;
  /** Drop queued work. In-flight tasks finish but nothing new is started. */
  clear(): void;
}

/**
 * Run `fn` for each distinct key, at most `concurrency` at a time.
 * A key is attempted once per pool lifetime (success or failure).
 */
export function createKeyedPool<K>(options: {
  concurrency: number;
  keyOf?: (key: K) => string;
  run: (key: K) => Promise<void>;
}): KeyedPool<K> {
  const concurrency = Math.max(1, options.concurrency);
  const keyOf = options.keyOf ?? ((key: K) => String(key));
  const seen = new Set<string>();
  const waiting: K[] = [];
  let active = 0;
  let stopped = false;

  function pump(): void {
    while (!stopped && active < concurrency && waiting.length > 0) {
      const item = waiting.shift();
      if (item === undefined) return;
      active += 1;
      void options
        .run(item)
        .catch(() => {
          // Caller decides whether to surface the error. The pool must not
          // throw: one failed key must not stall the rest of the queue.
        })
        .finally(() => {
          active -= 1;
          pump();
        });
    }
  }

  return {
    enqueue(keys) {
      if (stopped) return;
      for (const key of keys) {
        const id = keyOf(key);
        if (seen.has(id)) continue;
        seen.add(id);
        waiting.push(key);
      }
      pump();
    },
    clear() {
      stopped = true;
      waiting.length = 0;
    },
  };
}
