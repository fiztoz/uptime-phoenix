import { expect, test } from "bun:test";
import { createSessionResource } from "./session-resource";

test("shares pending reads, isolates copies, and expires first-paint data", async () => {
  let calls = 0;
  let clock = 0;
  const resource = createSessionResource(
    async () => {
      calls++;
      return [{ name: "folder" }];
    },
    () => "session",
    () => clock,
  );
  expect(resource.peek()).toBeUndefined();
  const [a, b] = await Promise.all([resource.refresh(), resource.refresh()]);
  expect(calls).toBe(1);
  a[0].name = "edited";
  expect(b[0].name).toBe("folder");
  expect(resource.peek()?.[0].name).toBe("folder");
  clock = 60_000;
  expect(resource.peek()).toBeUndefined();
  await resource.refresh();
  expect(calls).toBe(2);
});

test("session changes cannot publish an old pending response", async () => {
  let owner: string | null = "alice";
  let finish!: (value: string[]) => void;
  const resource = createSessionResource(
    () =>
      new Promise<string[]>((resolve) => {
        finish = resolve;
      }),
    () => owner,
  );
  const pending = resource.refresh();
  owner = "bob";
  expect(resource.peek()).toBeUndefined();
  finish(["private"]);
  await expect(pending).rejects.toThrow("Session changed");
  expect(resource.peek()).toBeUndefined();
  owner = null;
  expect(resource.peek()).toBeUndefined();
});

test("writes invalidate pending reads and force callers onto the fresh result", async () => {
  let finish!: (value: string[]) => void;
  let calls = 0;
  const resource = createSessionResource(
    () => {
      calls++;
      return calls === 1
        ? new Promise<string[]>((resolve) => {
            finish = resolve;
          })
        : Promise.resolve(["new"]);
    },
    () => "session",
  );
  const old = resource.refresh();
  resource.clear();
  finish(["old"]);
  expect(await old).toEqual(["new"]);
  expect(resource.peek()).toEqual(["new"]);
  expect(calls).toBe(2);
});

test("failed refresh keeps prior data and can be retried; empty is cached", async () => {
  let fail = false;
  const resource = createSessionResource(
    async () => {
      if (fail) throw new Error("offline");
      return [];
    },
    () => "session",
  );
  await resource.refresh();
  fail = true;
  await expect(resource.refresh()).rejects.toThrow("offline");
  expect(resource.peek()).toEqual([]);
  fail = false;
  expect(await resource.refresh()).toEqual([]);
  resource.clear();
  expect(resource.peek()).toBeUndefined();
});
