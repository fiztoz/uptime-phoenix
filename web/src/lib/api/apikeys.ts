/**
 * API key management wrappers (create returns one-time plaintext key).
 */
import { api } from "./client";

export type ApiKeyScope = "read" | "write" | "metrics";

export interface ApiKey {
  id: number;
  user_id?: number;
  name: string;
  active: boolean;
  scopes: ApiKeyScope[];
  expires_at?: string | null;
  last_used_at?: string | null;
  created_at: string;
}

export interface CreateApiKeyInput {
  name: string;
  scopes?: ApiKeyScope[];
  expires_at?: string | null;
}

/** Response when a key is created — plaintext shown once. */
export interface CreateApiKeyResponse {
  api_key: ApiKey;
  key: string;
}

export const apiKeysApi = {
  async list(): Promise<ApiKey[]> {
    return api.get<ApiKey[]>("/api-keys");
  },

  async create(input: CreateApiKeyInput): Promise<CreateApiKeyResponse> {
    return api.post<CreateApiKeyResponse>("/api-keys", input);
  },

  async revoke(id: number): Promise<void> {
    return api.del(`/api-keys/${id}`);
  },
};
