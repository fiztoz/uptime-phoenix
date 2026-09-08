/**
 * Tags CRUD and monitor tag assignment API wrappers.
 */
import { api } from "./client";
import { createSessionResource } from "../session-resource";

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

export const tagsCatalog = createSessionResource(
  () => api.get<Tag[]>("/tags"),
  () => (typeof localStorage === "undefined" ? null : api.getAuthHeader()),
);

export const tagsApi = {
  async list(): Promise<Tag[]> {
    return tagsCatalog.refresh();
  },

  async get(id: number): Promise<Tag> {
    return api.get<Tag>(`/tags/${id}`);
  },

  async create(input: CreateTagInput): Promise<Tag> {
    const result = await api.post<Tag>("/tags", input);
    tagsCatalog.clear();
    return result;
  },

  async update(id: number, input: UpdateTagInput): Promise<Tag> {
    const result = await api.put<Tag>(`/tags/${id}`, input);
    tagsCatalog.clear();
    return result;
  },

  async remove(id: number): Promise<void> {
    await api.del(`/tags/${id}`);
    tagsCatalog.clear();
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
