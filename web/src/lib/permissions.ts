/**
 * Frontend permission gates — single source for hiding create/edit/delete UI.
 *
 * Every helper mirrors the server's enforcement (services.AccessService and
 * the router's RequireCapability gates in internal/adapters/http/router.go):
 *
 *   monitors     — create needs `is_admin || can_create_monitors`; edit/delete
 *                  one monitor needs `is_admin || monitor.user_id === me.id`
 *                  (ownership, NOT the create flag — see UserView docs in
 *                  internal/adapters/http/handlers/auth.go).
 *   groups       — create needs `is_admin || can_create_groups`; per-group
 *                  edit/delete come from the server-computed `can_edit` /
 *                  `can_edit_metadata` on MonitorGroupView, not from here.
 *   notifications (incl. templates + escalation policies) — any mutation needs
 *                  `is_admin || can_manage_notifications`.
 *   maintenance  — any mutation needs `is_admin || can_manage_maintenance`.
 *   tags / proxies / api-keys / backup / status-pages / users — admin-only.
 *
 * Capability flags are RAW (an admin holds every flag false yet may do
 * everything), so every helper ORs `is_admin` first. Never gate on a flag
 * alone. A missing `user_id` (snapshot cached before the WS wire carried it)
 * fails closed: only an admin sees the button.
 */

export interface CapabilityUser {
  id: number;
  is_admin?: boolean;
  can_manage_notifications?: boolean;
  can_manage_maintenance?: boolean;
  can_create_monitors?: boolean;
  can_create_top_level_monitors?: boolean;
  can_create_groups?: boolean;
  can_edit_group_metadata?: boolean;
}

export interface OwnedMonitor {
  user_id?: number | null;
}

/** Non-null user that is an admin. */
export function isAdmin(user: CapabilityUser | null | undefined): boolean {
  return user?.is_admin ?? false;
}

/** May mutate notifications, templates, and escalation policies. */
export function canManageNotifications(
  user: CapabilityUser | null | undefined,
): boolean {
  return isAdmin(user) || (user?.can_manage_notifications ?? false);
}

/** May mutate maintenance windows and their monitor assignments. */
export function canManageMaintenance(
  user: CapabilityUser | null | undefined,
): boolean {
  return isAdmin(user) || (user?.can_manage_maintenance ?? false);
}

/** May open the "new monitor" form (placement still enforced per group). */
export function canCreateMonitors(
  user: CapabilityUser | null | undefined,
): boolean {
  return isAdmin(user) || (user?.can_create_monitors ?? false);
}

/** May open the "new group" form. */
export function canCreateGroups(
  user: CapabilityUser | null | undefined,
): boolean {
  return isAdmin(user) || (user?.can_create_groups ?? false);
}

/**
 * May edit/clone-gated-members/pause/delete THIS monitor: admin or its
 * creator. Deliberately ignores `can_create_monitors` — being able to make
 * monitors is not being able to touch other people's (the server 403s that
 * save, so showing the button only leads to a dead end).
 */
export function canEditMonitor(
  user: CapabilityUser | null | undefined,
  monitor: OwnedMonitor | null | undefined,
): boolean {
  if (!user || !monitor) return false;
  if (user.is_admin) return true;
  return monitor.user_id != null && monitor.user_id === user.id;
}
