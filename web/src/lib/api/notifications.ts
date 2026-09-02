/**
 * Notification CRUD + test + assignment API wrappers.
 * Matches backend endpoints defined in task.
 */
import { api } from "./client";

export interface Notification {
  id: number;
  name: string;
  type: string;
  config: Record<string, unknown>;
  active: boolean;
  /**
   * When true, auto-linked to every new MONITOR owned by this user.
   * Monitors only — a monitor GROUP is never auto-attached, on either group
   * create or notification create. Group links are always explicit
   * (see $lib/group-notifications).
   */
  is_default: boolean;
  /**
   * When true, DOWN messages include the public acknowledgement deep-link
   * (Discord: Acknowledge button; other providers: a text link). Default off.
   */
  include_ack_url: boolean;
  /** Reusable provider-specific message layout, or null for the built-in layout. */
  template_id: number | null;
  created_at: string;
  updated_at: string;
}

export interface CreateNotificationInput {
  name: string;
  type: string;
  config: Record<string, unknown>;
  active?: boolean;
  is_default?: boolean;
  include_ack_url?: boolean;
  template_id?: number | null;
}

export interface UpdateNotificationInput extends Partial<CreateNotificationInput> {}

export const notificationsApi = {
  async list(): Promise<Notification[]> {
    return api.get<Notification[]>("/notifications");
  },

  async get(id: number): Promise<Notification> {
    return api.get<Notification>(`/notifications/${id}`);
  },

  async create(input: CreateNotificationInput): Promise<Notification> {
    return api.post<Notification>("/notifications", input);
  },

  async update(
    id: number,
    input: UpdateNotificationInput,
  ): Promise<Notification> {
    return api.put<Notification>(`/notifications/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/notifications/${id}`);
  },

  async test(id: number): Promise<void> {
    return api.post(`/notifications/${id}/test`);
  },

  // Assignment helpers (paths match backend routes under /api/notifications for consistency)
  async listForMonitor(monitorId: number): Promise<Notification[]> {
    return api.get<Notification[]>(`/monitors/${monitorId}/notifications`);
  },

  async assignToMonitor(
    monitorId: number,
    notificationId: number,
  ): Promise<void> {
    return api.post(`/notifications/${notificationId}/monitor/${monitorId}`);
  },

  async unassignFromMonitor(
    monitorId: number,
    notificationId: number,
  ): Promise<void> {
    return api.del(`/notifications/${notificationId}/monitor/${monitorId}`);
  },

  // Monitor-group links. A group alerts on its OWN derived status (its rollup
  // condition), so these are NOT a shortcut for attaching to every monitor in
  // the group. Never called implicitly: an `is_default` notification is never
  // auto-attached to a group. See $lib/group-notifications.
  // Read side lives on monitorGroupsApi.listNotifications (GET /monitor-groups/:id/notifications).
  async attachToGroup(notificationId: number, groupId: number): Promise<void> {
    return api.post(`/notifications/${notificationId}/group/${groupId}`);
  },

  async detachFromGroup(
    notificationId: number,
    groupId: number,
  ): Promise<void> {
    return api.del(`/notifications/${notificationId}/group/${groupId}`);
  },
};
