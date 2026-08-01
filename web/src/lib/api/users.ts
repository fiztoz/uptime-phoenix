/**
 * User management wrappers (admin action — create/list/update/delete users).
 */
import { api } from "./client";

export interface User {
  id: number;
  username: string;
  active: boolean;
  is_admin: boolean;
  /**
   * The four capability flags are RAW — an admin has every one of them false and
   * may still do everything. Gate on `is_admin || can_x`, never on the flag
   * alone, exactly as the server's AccessService does.
   */
  can_manage_notifications: boolean;
  can_manage_maintenance: boolean;
  /**
   * Permission to CREATE. Not permission to edit a monitor already on screen —
   * that is per-resource ownership (`monitor.user_id === me.id`), which no
   * user-level flag can answer. See canEditMonitor in $lib/permissions.
   */
  can_create_monitors: boolean;
  /** Place new monitors with no group (group_id null). Needs can_create_monitors. */
  can_create_top_level_monitors: boolean;
  can_create_groups: boolean;
  /** Edit group metadata on visible folders; not name/parent/delete. */
  can_edit_group_metadata: boolean;
  timezone: string;
  two_factor_enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateUserInput {
  username: string;
  password: string;
  active?: boolean;
  is_admin?: boolean;
  can_manage_notifications?: boolean;
  can_manage_maintenance?: boolean;
  can_create_monitors?: boolean;
  can_create_top_level_monitors?: boolean;
  can_create_groups?: boolean;
  can_edit_group_metadata?: boolean;
  timezone?: string;
}

export type UpdateUserInput = Partial<{
  username: string;
  active: boolean;
  is_admin: boolean;
  can_manage_notifications: boolean;
  can_manage_maintenance: boolean;
  can_create_monitors: boolean;
  can_create_top_level_monitors: boolean;
  can_create_groups: boolean;
  can_edit_group_metadata: boolean;
  timezone: string;
  password: string;
}>;

/**
 * One group grant: which folder, and how far down it reaches.
 *
 * include_descendants=true covers the folder, its subfolders, and every monitor
 * in any of them. false covers the folder and the monitors filed directly in it,
 * and stops there.
 */
export interface GroupGrant {
  group_id: number;
  include_descendants: boolean;
}

export interface UserPermissions {
  monitor_ids: number[];
  groups: GroupGrant[];
}

export const usersApi = {
  async list(): Promise<User[]> {
    return api.get<User[]>("/users");
  },

  async create(input: CreateUserInput): Promise<{ user: User }> {
    return api.post<{ user: User }>("/users", input);
  },

  async update(id: number, patch: UpdateUserInput): Promise<{ user: User }> {
    return api.put<{ user: User }>(`/users/${id}`, patch);
  },

  async getPermissions(id: number): Promise<UserPermissions> {
    return api.get<UserPermissions>(`/users/${id}/permissions`);
  },

  async updatePermissions(
    id: number,
    permissions: UserPermissions,
  ): Promise<UserPermissions> {
    return api.put<UserPermissions>(`/users/${id}/permissions`, permissions);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/users/${id}`);
  },
};
