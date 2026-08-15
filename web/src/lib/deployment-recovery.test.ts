import { describe, expect, test } from "bun:test";
import {
  PRELOAD_RELOAD_COOLDOWN_MS,
  recoverFromPreloadError,
} from "./deployment-recovery";

function memoryStorage(): Pick<Storage, "getItem" | "setItem"> {
  const values = new Map<string, string>();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
}

describe("deployment preload recovery", () => {
  test("turns a stale chunk preload failure into one hard reload", () => {
    let prevented = false;
    let reloads = 0;

    const recovered = recoverFromPreloadError(
      { preventDefault: () => (prevented = true) },
      {
        storage: memoryStorage(),
        now: () => 1_000,
        reload: () => reloads++,
      },
    );

    expect(recovered).toBe(true);
    expect(prevented).toBe(true);
    expect(reloads).toBe(1);
  });

  test("does not enter a reload loop when the replacement is still rolling out", () => {
    const storage = memoryStorage();
    let reloads = 0;

    recoverFromPreloadError(
      { preventDefault: () => undefined },
      { storage, now: () => 1_000, reload: () => reloads++ },
    );

    let secondPrevented = false;
    const recovered = recoverFromPreloadError(
      { preventDefault: () => (secondPrevented = true) },
      {
        storage,
        now: () => 1_000 + PRELOAD_RELOAD_COOLDOWN_MS - 1,
        reload: () => reloads++,
      },
    );

    expect(recovered).toBe(false);
    expect(secondPrevented).toBe(false);
    expect(reloads).toBe(1);
  });
});
