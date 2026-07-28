/**
 * Typed API client for Phoenix backend.
 * Base URL from env (default /api). Attaches auth header automatically.
 * 401 clears auth and redirects to login. Structured error handling.
 */

const API_BASE = import.meta.env.VITE_API_BASE || "/api";

interface ApiError {
  message: string;
  code?: string;
  details?: unknown;
}

class ApiClient {
  private getAuth(): string | null {
    return localStorage.getItem("phoenix_jwt");
  }

  private setAuth(val: string): void {
    localStorage.setItem("phoenix_jwt", val);
  }

  private clearAuth(): void {
    localStorage.removeItem("phoenix_jwt");
  }

  private getHeaders(): HeadersInit {
    const headers: HeadersInit = { "Content-Type": "application/json" };
    const auth = this.getAuth();
    if (auth) {
      (headers as Record<string, string>).Authorization = `Bearer ${auth}`;
    }
    return headers;
  }

  private async handleResponse<T>(response: Response): Promise<T> {
    if (response.status === 204) return undefined as T;

    const ct = response.headers.get("content-type");
    const data = ct?.includes("json")
      ? await response.json()
      : await response.text();

    if (!response.ok) {
      const body = data as Record<string, unknown>;
      const err: ApiError = {
        message:
          (typeof body?.message === "string" && body.message) ||
          (typeof body?.error === "string" && body.error) ||
          `HTTP ${response.status}`,
        code: typeof body?.code === "string" ? body.code : undefined,
        details: data,
      };
      if (response.status === 401) {
        this.clearAuth();
        if (
          typeof window !== "undefined" &&
          !location.pathname.startsWith("/login")
        ) {
          location.href = "/login";
        }
      }
      throw err;
    }
    return data as T;
  }

  async get<T>(
    path: string,
    params?: Record<string, string | number | boolean>,
  ): Promise<T> {
    let url = `${API_BASE}${path}`;
    if (params) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(params))
        if (v != null) qs.append(k, String(v));
      if (qs.toString()) url += `?${qs}`;
    }
    const res = await fetch(url, { method: "GET", headers: this.getHeaders() });
    return this.handleResponse<T>(res);
  }

  async post<T>(path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
      method: "POST",
      headers: this.getHeaders(),
      body: body ? JSON.stringify(body) : undefined,
    });
    return this.handleResponse<T>(res);
  }

  async put<T>(path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
      method: "PUT",
      headers: this.getHeaders(),
      body: body ? JSON.stringify(body) : undefined,
    });
    return this.handleResponse<T>(res);
  }

  async del(path: string): Promise<void> {
    const res = await fetch(`${API_BASE}${path}`, {
      method: "DELETE",
      headers: this.getHeaders(),
    });
    return this.handleResponse<void>(res);
  }

  setAuthHeader(val: string) {
    this.setAuth(val);
  }
  clearAuthHeader() {
    this.clearAuth();
  }
  getAuthHeader() {
    return this.getAuth();
  }
}

export const api = new ApiClient();
export type { ApiError };
