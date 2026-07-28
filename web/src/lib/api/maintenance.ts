/**
 * Maintenance window CRUD API wrappers.
 */
import { api } from "./client";

export type MaintenanceStrategy = "single" | "cron";

export interface MaintenanceWindow {
  id: number;
  user_id?: number;
  title: string;
  description: string;
  active: boolean;
  strategy: MaintenanceStrategy;
  start_date: string;
  end_date: string;
  cron_expr?: string;
  duration?: number;
  /** IANA timezone for cron evaluation (default "UTC"). */
  timezone?: string;
  monitor_ids?: number[];
}

export interface CreateMaintenanceInput {
  title: string;
  description?: string;
  active?: boolean;
  strategy: MaintenanceStrategy;
  start_date?: string;
  end_date?: string;
  cron_expr?: string;
  duration?: number;
  /** IANA timezone for cron evaluation (default "UTC"). */
  timezone?: string;
  monitor_ids?: number[];
}

export interface UpdateMaintenanceInput extends Partial<CreateMaintenanceInput> {}

export const maintenanceApi = {
  async list(): Promise<MaintenanceWindow[]> {
    return api.get<MaintenanceWindow[]>("/maintenance");
  },

  async get(id: number): Promise<MaintenanceWindow> {
    return api.get<MaintenanceWindow>(`/maintenance/${id}`);
  },

  async create(input: CreateMaintenanceInput): Promise<MaintenanceWindow> {
    return api.post<MaintenanceWindow>("/maintenance", input);
  },

  async update(
    id: number,
    input: UpdateMaintenanceInput,
  ): Promise<MaintenanceWindow> {
    return api.put<MaintenanceWindow>(`/maintenance/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/maintenance/${id}`);
  },
};
