/**
 * Auth API wrappers — typed calls to /api/auth/*
 */
import { api, type ApiError } from "./client";

export interface User {
  id: number;
  username: string;
  is_admin?: boolean;
  can_manage_notifications?: boolean;
  can_manage_maintenance?: boolean;
  can_view_extensions?: boolean;
  can_create_monitors?: boolean;
  can_create_top_level_monitors?: boolean;
  can_create_groups?: boolean;
  can_edit_group_metadata?: boolean;
  active?: boolean;
  totp_enabled?: boolean;
  two_factor_enabled?: boolean;
}

/** Normalize backend user payload (totp_enabled / two_factor_enabled). */
export function normalizeUser(raw: User): User {
  const enabled = raw.two_factor_enabled ?? raw.totp_enabled ?? false;
  return { ...raw, two_factor_enabled: enabled, totp_enabled: enabled };
}

export interface LoginResponse {
  requires_2fa?: boolean;
  ticket?: string;
  token?: string;
  user?: User;
}

export interface RegisterResponse {
  token: string;
  user: User;
}

export interface Setup2FAResponse {
  secret: string;
  qr_url: string;
}

export interface OIDCStatus {
  enabled: boolean;
}

export const authApi = {
  async register(
    username: string,
    password: string,
  ): Promise<RegisterResponse> {
    return api.post<RegisterResponse>("/auth/register", { username, password });
  },

  async login(username: string, password: string): Promise<LoginResponse> {
    return api.post<LoginResponse>("/auth/login", { username, password });
  },

  async verify2FA(
    ticket: string,
    token: string,
  ): Promise<{ token: string; user: User }> {
    return api.post("/auth/verify-2fa", { ticket, token });
  },

  async me(): Promise<{ user: User }> {
    const res = await api.get<{ user: User }>("/auth/me");
    return { user: normalizeUser(res.user) };
  },

  async hasUsers(): Promise<{ has_users: boolean }> {
    return api.get("/auth/has-users");
  },

  /** Public: whether OIDC SSO is configured. Never returns secrets. */
  async oidcStatus(): Promise<OIDCStatus> {
    return api.get<OIDCStatus>("/auth/oidc/status");
  },

  /**
   * Browser navigation target for the OIDC authorize redirect.
   * Absolute path so it works whether the SPA is served from / or a subpath.
   */
  oidcLoginURL(): string {
    return "/api/auth/oidc/login";
  },

  async setup2FA(): Promise<Setup2FAResponse> {
    return api.post("/auth/setup-2fa");
  },

  async enable2FA(otpToken: string): Promise<void> {
    return api.post("/auth/enable-2fa", { token: otpToken });
  },

  async disable2FA(password: string): Promise<void> {
    return api.post("/auth/disable-2fa", { password });
  },
};
