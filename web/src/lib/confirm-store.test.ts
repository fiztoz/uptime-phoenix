/// <reference types="bun-types" />
import { afterEach, describe, expect, test } from "bun:test";

// Polyfill $state rune for unit test environment before importing Svelte store module.
try {
  (globalThis as any).$state ??= <T>(v: T): T => v;
} catch {
  (globalThis as any).$state = <T>(v: T): T => v;
}

const { confirmAction, confirmController } = await import(
  "./stores/confirm.svelte"
);

// The controller is a module-level singleton, so a test that leaves a dialog
// open would leak into the next one. Drain whatever is still pending.
afterEach(() => {
  while (confirmController.current) confirmController.cancel();
});

describe("confirm store — single request", () => {
  test("starts with nothing open", () => {
    expect(confirmController.current).toBeNull();
  });

  test("ask() opens the request and resolves true on confirm()", async () => {
    const answer = confirmAction({ title: 'Delete monitor "api"?' });

    expect(confirmController.current?.title).toBe('Delete monitor "api"?');

    confirmController.confirm();

    expect(await answer).toBe(true);
    expect(confirmController.current).toBeNull();
  });

  test("ask() resolves false on cancel()", async () => {
    const answer = confirmAction({ title: "Delete folder?" });

    confirmController.cancel();

    expect(await answer).toBe(false);
    expect(confirmController.current).toBeNull();
  });

  test("options reach the pending request untouched", () => {
    void confirmAction({
      title: "Erase all heartbeat history?",
      message: "This cannot be undone.",
      confirmLabel: "Erase history",
      cancelLabel: "Keep it",
      destructive: true,
      requireText: "api",
    });

    const pending = confirmController.current;
    expect(pending?.message).toBe("This cannot be undone.");
    expect(pending?.confirmLabel).toBe("Erase history");
    expect(pending?.cancelLabel).toBe("Keep it");
    expect(pending?.destructive).toBe(true);
    expect(pending?.requireText).toBe("api");
  });

  test("answering twice does not throw and stays closed", async () => {
    const answer = confirmAction({ title: "Delete?" });

    confirmController.confirm();
    // A double-fire (e.g. Enter keydown racing the button click) must be inert.
    confirmController.cancel();

    expect(await answer).toBe(true);
    expect(confirmController.current).toBeNull();
  });

  test("confirm()/cancel() with nothing open are no-ops", () => {
    expect(() => confirmController.confirm()).not.toThrow();
    expect(() => confirmController.cancel()).not.toThrow();
    expect(confirmController.current).toBeNull();
  });
});

describe("confirm store — queueing", () => {
  test("a second ask() while one is open does not replace the open one", () => {
    void confirmAction({ title: "First" });
    void confirmAction({ title: "Second" });

    expect(confirmController.current?.title).toBe("First");
  });

  test("settling the open request promotes the queued one", async () => {
    const first = confirmAction({ title: "First" });
    const second = confirmAction({ title: "Second" });

    confirmController.confirm();
    expect(confirmController.current?.title).toBe("Second");

    confirmController.confirm();
    expect(confirmController.current).toBeNull();

    expect(await first).toBe(true);
    expect(await second).toBe(true);
  });

  // The load-bearing test. If settle() answered the wrong pending request,
  // cancelling the dialog on screen would confirm a *different* delete.
  test("each answer resolves its own promise, not its neighbour's", async () => {
    const settled: string[] = [];
    const record = (name: string) => (confirmed: boolean) => {
      settled.push(`${name}:${confirmed}`);
      return confirmed;
    };

    const deleteMonitor = confirmAction({ title: "Delete monitor?" }).then(
      record("monitor"),
    );
    const deleteFolder = confirmAction({ title: "Delete folder?" }).then(
      record("folder"),
    );

    // The user cancels the dialog they can see — the monitor one.
    expect(confirmController.current?.title).toBe("Delete monitor?");
    confirmController.cancel();

    // ...and confirms the folder one that took its place.
    expect(confirmController.current?.title).toBe("Delete folder?");
    confirmController.confirm();

    expect(await deleteMonitor).toBe(false);
    expect(await deleteFolder).toBe(true);
    // Answers land on the right promise, oldest first.
    expect(settled).toEqual(["monitor:false", "folder:true"]);
  });

  test("a queue deeper than two stays FIFO", async () => {
    const answers = ["A", "B", "C", "D"].map((title) =>
      confirmAction({ title }),
    );

    expect(confirmController.current?.title).toBe("A");

    confirmController.confirm(); // A
    expect(confirmController.current?.title).toBe("B");

    confirmController.cancel(); // B
    expect(confirmController.current?.title).toBe("C");

    confirmController.cancel(); // C
    expect(confirmController.current?.title).toBe("D");

    confirmController.confirm(); // D
    expect(confirmController.current).toBeNull();

    expect(await Promise.all(answers)).toEqual([true, false, false, true]);
  });

  test("a request queued while another is open is not itself openable early", async () => {
    const first = confirmAction({ title: "First" });
    const second = confirmAction({ title: "Second" });
    const third = confirmAction({ title: "Third" });

    // Only one dialog is ever on screen.
    expect(confirmController.current?.title).toBe("First");
    confirmController.cancel();
    expect(confirmController.current?.title).toBe("Second");

    // A brand-new ask() arriving now goes behind the ones already waiting.
    const fourth = confirmAction({ title: "Fourth" });

    confirmController.confirm(); // Second
    expect(confirmController.current?.title).toBe("Third");
    confirmController.cancel(); // Third
    expect(confirmController.current?.title).toBe("Fourth");
    confirmController.confirm(); // Fourth

    expect(await Promise.all([first, second, third, fourth])).toEqual([
      false,
      true,
      false,
      true,
    ]);
  });
});
