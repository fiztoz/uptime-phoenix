/**
 * Outbound proxy CRUD API wrappers.
 *
 * Mirrors internal/adapters/http/handlers/proxy.go's ProxyView — note that
 * Password is NEVER present on read (see AGENTS.md Wire-Shape Discipline);
 * it is write-only on create/update.
 */
import { api } from "./client";

export type ProxyProtocol = "http" | "https" | "socks5";

export interface Proxy {
  id: number;
  user_id: number;
  protocol: ProxyProtocol;
  host: string;
  port: number;
  auth: boolean;
  username: string;
  active: boolean;
  is_default: boolean;
}

export interface UpsertProxyInput {
  protocol: ProxyProtocol;
  host: string;
  port: number;
  auth: boolean;
  username?: string;
  /** Write-only: omit on update to keep the existing credential unchanged. */
  password?: string;
  active?: boolean;
  is_default?: boolean;
}

export const proxiesApi = {
  async list(): Promise<Proxy[]> {
    return api.get<Proxy[]>("/proxies");
  },

  async create(input: UpsertProxyInput): Promise<Proxy> {
    return api.post<Proxy>("/proxies", input);
  },

  async update(id: number, input: UpsertProxyInput): Promise<Proxy> {
    return api.put<Proxy>(`/proxies/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/proxies/${id}`);
  },
};
