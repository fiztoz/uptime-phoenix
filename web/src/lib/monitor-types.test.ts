import { describe, expect, test } from "bun:test";
import { monitorTypeConfig } from "./monitor-types";

describe("HTTP monitor form metadata", () => {
  test("places every HTTP field in a declared section", () => {
    const http = monitorTypeConfig.http;
    const sectionIDs = new Set(http.sections?.map((section) => section.id));

    expect(sectionIDs.size).toBeGreaterThan(0);
    for (const field of http.fields) {
      expect(field.section).toBeTruthy();
      expect(sectionIDs.has(field.section as string)).toBe(true);
    }
  });

  test("offers guided JSON comparisons and reveals expected value when needed", () => {
    const fields = monitorTypeConfig.http.fields;
    const syntax = fields.find((field) => field.key === "json_query_syntax");
    const operator = fields.find((field) => field.key === "json_operator");
    const expected = fields.find((field) => field.key === "expected_value");

    expect(syntax?.default).toBe("gjson");
    expect(syntax?.options?.map((option) => option.value)).toEqual([
      "gjson",
      "jsonpath",
    ]);
    expect(operator?.default).toBe("exists");
    expect(operator?.options?.map((option) => option.value)).toEqual([
      "exists",
      "has_value",
      "not_exists",
      "equals",
      "not_equals",
      "contains",
      "not_contains",
    ]);
    expect(expected?.showWhen?.values).toEqual([
      "equals",
      "not_equals",
      "contains",
      "not_contains",
    ]);
  });
});

describe("Database monitor form metadata", () => {
  const db = monitorTypeConfig.database;

  test("places every database field in a declared section", () => {
    const sectionIDs = new Set(db.sections?.map((section) => section.id));

    expect(sectionIDs.size).toBeGreaterThan(0);
    for (const field of db.fields) {
      expect(field.section).toBeTruthy();
      expect(sectionIDs.has(field.section as string)).toBe(true);
    }
  });

  test("exposes locked capacity config keys", () => {
    const byKey = Object.fromEntries(
      db.fields.map((field) => [field.key, field]),
    );

    expect(byKey.check_session_pool?.type).toBe("checkbox");
    expect(byKey.check_session_pool?.default).toBe(false);
    expect(byKey.session_pool_threshold?.type).toBe("number");
    expect(byKey.session_pool_threshold?.default).toBe(80);
    expect(byKey.check_storage?.type).toBe("checkbox");
    expect(byKey.check_storage?.default).toBe(false);
    expect(byKey.storage_threshold?.type).toBe("number");
    expect(byKey.storage_threshold?.default).toBe(80);
    expect(byKey.storage_max_gb?.type).toBe("number");
    expect(byKey.storage_max_gb?.default).toBeUndefined();
    expect(byKey.capacity_check_interval).toBeUndefined();
  });

  test("reveals session pool threshold only when the session-pool check is on", () => {
    const field = db.fields.find(
      (item) => item.key === "session_pool_threshold",
    );
    expect(field?.showWhen).toEqual({
      key: "check_session_pool",
      values: ["true"],
    });
  });

  test("reveals storage fields only when the storage check is on", () => {
    for (const key of ["storage_threshold", "storage_max_gb"]) {
      const field = db.fields.find((item) => item.key === key);
      expect(field?.showWhen).toEqual({
        key: "check_storage",
        values: ["true"],
      });
    }
  });

  test("health_check options remain only ping and select_1", () => {
    const field = db.fields.find((item) => item.key === "health_check");
    expect(field?.type).toBe("select");
    expect(field?.options?.map((option) => option.value)).toEqual([
      "ping",
      "select_1",
    ]);
    expect(db.fields.some((item) => item.type === "textarea")).toBe(false);
    expect(db.fields.map((item) => item.key)).not.toContain("query");
  });
});
