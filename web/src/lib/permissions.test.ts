import { describe, expect, it } from "bun:test";
import {
  canCreateGroups,
  canCreateMonitors,
  canEditMonitor,
  canManageMaintenance,
  canManageNotifications,
  isAdmin,
} from "./permissions";

const admin = { id: 1, is_admin: true };
const creator = { id: 2, is_admin: false, can_create_monitors: true };
const viewer = { id: 3, is_admin: false };

describe("permissions", () => {
  it("admin passes every gate with all flags false (flags are RAW)", () => {
    expect(isAdmin(admin)).toBe(true);
    expect(canManageNotifications(admin)).toBe(true);
    expect(canManageMaintenance(admin)).toBe(true);
    expect(canCreateMonitors(admin)).toBe(true);
    expect(canCreateGroups(admin)).toBe(true);
    expect(canEditMonitor(admin, { user_id: 999 })).toBe(true);
    // Missing user_id still allows admin (fails closed only for non-admins).
    expect(canEditMonitor(admin, {})).toBe(true);
  });

  it("capability flags gate their own domain only", () => {
    const notif = { id: 4, is_admin: false, can_manage_notifications: true };
    expect(canManageNotifications(notif)).toBe(true);
    expect(canManageMaintenance(notif)).toBe(false);
    expect(canCreateMonitors(notif)).toBe(false);

    const maint = { id: 5, is_admin: false, can_manage_maintenance: true };
    expect(canManageMaintenance(maint)).toBe(true);
    expect(canManageNotifications(maint)).toBe(false);
  });

  it("edit is ownership, not the create flag", () => {
    // Creator edits their own monitor.
    expect(canEditMonitor(creator, { user_id: 2 })).toBe(true);
    // Creator does NOT edit someone else's, despite holding can_create_monitors.
    expect(canEditMonitor(creator, { user_id: 7 })).toBe(false);
    // Plain viewer edits nothing.
    expect(canEditMonitor(viewer, { user_id: 3 })).toBe(true);
    expect(canEditMonitor(viewer, { user_id: 2 })).toBe(false);
  });

  it("missing user_id fails closed for non-admins", () => {
    expect(canEditMonitor(creator, {})).toBe(false);
    expect(canEditMonitor(creator, { user_id: null })).toBe(false);
    expect(canEditMonitor(creator, null)).toBe(false);
    expect(canEditMonitor(null, { user_id: 2 })).toBe(false);
  });

  it("null user sees nothing", () => {
    expect(canManageNotifications(null)).toBe(false);
    expect(canManageMaintenance(null)).toBe(false);
    expect(canCreateMonitors(undefined)).toBe(false);
    expect(canCreateGroups(undefined)).toBe(false);
  });
});
