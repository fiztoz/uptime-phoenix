/** Short-lived first-paint data, shared across routes but never persisted. */
export function createSessionResource<T>(
  fetchValue: () => Promise<T>,
  owner: () => string | null,
  now = Date.now,
) {
  let session: string | null = null;
  let value: T | undefined;
  let expires = 0;
  let generation = 0;
  let pending: Promise<T> | undefined;

  function clear() {
    value = undefined;
    expires = 0;
    pending = undefined;
    generation++;
  }

  function syncSession() {
    const next = owner();
    if (next !== session) {
      clear();
      session = next;
    }
    return next;
  }

  function peek(): T | undefined {
    if (!syncSession() || now() >= expires) return undefined;
    return structuredClone(value);
  }

  // Every visit revalidates. Only simultaneous requests are coalesced.
  async function refresh(): Promise<T> {
    const startedSession = syncSession();
    const startedGeneration = generation;
    if (!pending) {
      pending = fetchValue()
        .then((result) => {
          if (
            owner() === startedSession &&
            generation === startedGeneration &&
            startedSession
          ) {
            value = structuredClone(result);
            expires = now() + 60_000;
          }
          return result;
        })
        .finally(() => {
          if (generation === startedGeneration) pending = undefined;
        });
    }
    const result = await pending;
    if (owner() !== startedSession) throw new Error("Session changed");
    // A write invalidated an older read: join the post-write refresh.
    if (generation !== startedGeneration) return refresh();
    return structuredClone(result);
  }

  return { peek, refresh, clear };
}
