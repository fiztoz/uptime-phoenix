/**
 * Admin-only monitor types and options (#88). The API refuses docker and
 * database monitors, proxy assignment and save_body_* for non-admins (403), so
 * the form must not offer them; these tests pin the data the form gates on.
 */
import { describe, expect, test } from "bun:test";
import {
  ADMIN_ONLY_MONITOR_TYPES,
  buildMonitorTypeSelectGroups,
  monitorTypeConfig,
} from "./monitor-types";

const offered = (groups: ReturnType<typeof buildMonitorTypeSelectGroups>) =>
  groups.flatMap((g) => g.options.map((o) => o.value));

describe("admin-only monitor types", () => {
  test("admins are offered every type", () => {
    const types = offered(buildMonitorTypeSelectGroups());
    for (const t of ADMIN_ONLY_MONITOR_TYPES) expect(types).toContain(t);
  });

  test("non-admins are not offered docker or database", () => {
    const types = offered(
      buildMonitorTypeSelectGroups(undefined, { isAdmin: false }),
    );
    expect(types).not.toContain("docker");
    expect(types).not.toContain("database");
    expect(types).toContain("http");
  });

  test("editing keeps the monitor's own type selectable", () => {
    const types = offered(
      buildMonitorTypeSelectGroups(undefined, {
        isAdmin: false,
        keepType: "docker",
      }),
    );
    expect(types).toContain("docker");
    expect(types).not.toContain("database");
  });
});

describe("admin-only options", () => {
  test("save_body_* fields are admin-only", () => {
    const fields = monitorTypeConfig.http.fields.filter((f) =>
      f.key.startsWith("save_body_on_"),
    );
    expect(fields.map((f) => f.key).sort()).toEqual([
      "save_body_on_error",
      "save_body_on_success",
    ]);
    for (const f of fields) expect(f.adminOnly).toBe(true);
  });
});
