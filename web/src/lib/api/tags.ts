/**
 * Tags CRUD and monitor tag assignment API wrappers.
 */
import { api } from "./client";

export interface Tag {
  id: number;
  name: string;
  color: string;
}

export interface MonitorTag {
  id?: number;
  monitor_id: number;
  tag_id: number;
  value?: string;
  tag?: Tag;
}

export interface CreateTagInput {
  name: string;
  color?: string;
}

export interface UpdateTagInput extends Partial<CreateTagInput> {}

export const tagsApi = {
  async list(): Promise<Tag[]> {
    return api.get<Tag[]>("/tags");
  },

  async get(id: number): Promise<Tag> {
    return api.get<Tag>(`/tags/${id}`);
  },

  async create(input: CreateTagInput): Promise<Tag> {
    return api.post<Tag>("/tags", input);
  },

  async update(id: number, input: UpdateTagInput): Promise<Tag> {
    return api.put<Tag>(`/tags/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/tags/${id}`);
  },

  async listForMonitor(monitorId: number): Promise<MonitorTag[]> {
    return api.get<MonitorTag[]>(`/monitors/${monitorId}/tags`);
  },

  async assignToMonitor(
    monitorId: number,
    tagId: number,
    value?: string,
  ): Promise<void> {
    return api.post(`/monitors/${monitorId}/tags`, {
      tag_id: tagId,
      value: value ?? "",
    });
  },

  async unassignFromMonitor(monitorId: number, tagId: number): Promise<void> {
    return api.del(`/monitors/${monitorId}/tags/${tagId}`);
  },
};
