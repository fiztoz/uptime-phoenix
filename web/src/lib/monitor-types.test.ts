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
