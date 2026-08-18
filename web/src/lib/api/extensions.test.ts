import { describe, expect, test } from "bun:test";
import {
  extensionIconSrc,
  normalizeExtension,
  normalizeExtensions,
} from "./extensions";

describe("normalizeExtension", () => {
  test("keeps id, title, path, and derived icon and drops unknown fields", () => {
    expect(
      normalizeExtension({
        id: "demo",
        title: "Demo",
        path: "/ext/demo/",
        extra: "nope",
        secret: true,
      }),
    ).toEqual({
      id: "demo",
      title: "Demo",
      path: "/ext/demo/",
      icon: "/ext/demo/icon.svg",
    });
  });

  test("keeps a safe icon override", () => {
    expect(
      normalizeExtension({
        id: "demo",
        title: "Demo",
        path: "/storage",
        icon: "/storage/favicon.ico",
      }),
    ).toEqual({
      id: "demo",
      title: "Demo",
      path: "/storage",
      icon: "/storage/favicon.ico",
    });
  });

  test("rejects missing, empty, or non-string fields", () => {
    expect(normalizeExtension(null)).toBeNull();
    expect(normalizeExtension(undefined)).toBeNull();
    expect(normalizeExtension("demo")).toBeNull();
    expect(normalizeExtension({ id: "x", title: "T" })).toBeNull();
    expect(normalizeExtension({ id: 1, title: "T", path: "/" })).toBeNull();
    expect(normalizeExtension({ id: "", title: "T", path: "/x" })).toBeNull();
    expect(normalizeExtension({ id: "x", title: "", path: "/x" })).toBeNull();
    expect(normalizeExtension({ id: "x", title: "T", path: "" })).toBeNull();
  });
});

describe("extensionIconSrc", () => {
  test("defaults to {path}/icon.svg", () => {
    expect(extensionIconSrc("/storage")).toBe("/storage/icon.svg");
    expect(extensionIconSrc("/storage/")).toBe("/storage/icon.svg");
  });

  test("rejects remote and traversal icons", () => {
    expect(extensionIconSrc("/storage", "https://evil.example/x.png")).toBe(
      "/storage/icon.svg",
    );
    expect(extensionIconSrc("/storage", "//evil.example/x.png")).toBe(
      "/storage/icon.svg",
    );
    expect(extensionIconSrc("/storage", "/storage/../secret.svg")).toBe(
      "/storage/icon.svg",
    );
  });
});

describe("normalizeExtensions", () => {
  test("drops invalid entries and unknown fields", () => {
    expect(
      normalizeExtensions([
        { id: "a", title: "A", path: "/a", foo: 1 },
        { id: "", title: "B", path: "/b" },
        { not: "an extension" },
        null,
      ]),
    ).toEqual([{ id: "a", title: "A", path: "/a", icon: "/a/icon.svg" }]);
  });

  test("empty or non-array is []", () => {
    expect(normalizeExtensions(null)).toEqual([]);
    expect(normalizeExtensions(undefined)).toEqual([]);
    expect(normalizeExtensions({ id: "a" })).toEqual([]);
    expect(normalizeExtensions([])).toEqual([]);
  });
});
