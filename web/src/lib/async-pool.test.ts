import { describe, expect, test } from "bun:test";
import { createKeyedPool } from "./async-pool";

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

describe("createKeyedPool", () => {
  test("never runs more than concurrency tasks at once", async () => {
    let inFlight = 0;
    let peak = 0;
    const started: number[] = [];

    const pool = createKeyedPool<number>({
      concurrency: 2,
      run: async (id) => {
        inFlight += 1;
        peak = Math.max(peak, inFlight);
        started.push(id);
        await wait(20);
        inFlight -= 1;
      },
    });

    pool.enqueue([1, 2, 3, 4, 5]);
    await wait(150);

    expect(started).toEqual([1, 2, 3, 4, 5]);
    expect(peak).toBe(2);
    expect(inFlight).toBe(0);
  });

  test("ignores duplicate keys", async () => {
    const started: number[] = [];
    const pool = createKeyedPool<number>({
      concurrency: 3,
      run: async (id) => {
        started.push(id);
      },
    });

    pool.enqueue([1, 1, 2]);
    pool.enqueue([2, 3]);
    await wait(20);

    expect(started).toEqual([1, 2, 3]);
  });

  test("clear drops queued work but does not throw", async () => {
    const started: number[] = [];
    const pool = createKeyedPool<number>({
      concurrency: 1,
      run: async (id) => {
        started.push(id);
        await wait(30);
      },
    });

    pool.enqueue([1, 2, 3]);
    await wait(5);
    pool.clear();
    await wait(80);

    expect(started).toEqual([1]);
  });

  test("a failed key does not stall later keys", async () => {
    const started: number[] = [];
    const pool = createKeyedPool<number>({
      concurrency: 1,
      run: async (id) => {
        started.push(id);
        if (id === 1) throw new Error("boom");
      },
    });

    pool.enqueue([1, 2]);
    await wait(20);

    expect(started).toEqual([1, 2]);
  });
});
