/**
 * Auth store using runes. Manages user, JWT in localStorage, login/register/2FA flows.
 * Single source for authentication state.
 */
import { goto } from "$app/navigation";
import { authApi, normalizeUser, type User } from "$lib/api/auth";
import { api } from "$lib/api/client";
import { webauthnApi } from "$lib/api/webauthn";
import { toast } from "svelte-sonner";

function createAuthStore() {
  let user = $state<User | null>(null);
  const initialJwt =
    typeof localStorage !== "undefined"
      ? localStorage.getItem("phoenix_jwt")
      : null;
  let jwt = $state<string | null>(initialJwt);

  $effect.root(() => {
    $effect(() => {
      if (typeof localStorage === "undefined") return;
      if (jwt) {
        localStorage.setItem("phoenix_jwt", jwt);
        api.setAuthHeader(jwt);
      } else {
        localStorage.removeItem("phoenix_jwt");
        api.clearAuthHeader();
      }
    });
  });

  let isAuthenticated = $derived(user !== null);

  async function login(
    username: string,
    password: string,
  ): Promise<{ requires_2fa?: boolean; ticket?: string }> {
    try {
      const res = await authApi.login(username, password);
      if (res.requires_2fa && res.ticket) {
        return { requires_2fa: true, ticket: res.ticket };
      }
      if (res.token && res.user) {
        jwt = res.token;
        user = normalizeUser(res.user);
        toast.success("Logged in successfully");
        return {};
      }
      throw new Error("Unexpected login response");
    } catch (err: any) {
      const msg = err?.message || "Login failed";
      toast.error(msg);
      throw err;
    }
  }

  async function verify2FA(ticket: string, otp: string): Promise<void> {
    try {
      const res = await authApi.verify2FA(ticket, otp);
      if (res.token && res.user) {
        jwt = res.token;
        user = normalizeUser(res.user);
        toast.success("2FA verified");
      }
    } catch (err: any) {
      toast.error(err?.message || "2FA verification failed");
      throw err;
    }
  }

  async function passkeyLogin(username: string): Promise<void> {
    // Passwordless first-factor login: a valid passkey assertion yields a
    // session directly, reusing the same jwt/user assignment as 2FA verify.
    const res = await webauthnApi.login(username);
    if (res.token && res.user) {
      jwt = res.token;
      user = normalizeUser(res.user);
      toast.success("Signed in with passkey");
      return;
    }
    throw new Error("Unexpected passkey login response");
  }

  async function register(username: string, password: string): Promise<void> {
    try {
      const res = await authApi.register(username, password);
      jwt = res.token;
      user = normalizeUser(res.user);
      toast.success("Account created");
    } catch (err: any) {
      toast.error(err?.message || "Registration failed");
      throw err;
    }
  }

  /** Accept a session JWT issued by the OIDC callback handoff. */
  async function completeOIDC(token: string): Promise<void> {
    jwt = token;
    api.setAuthHeader(token);
    const res = await authApi.me();
    user = res.user;
    toast.success("Signed in with SSO");
  }

  function logout(): void {
    user = null;
    jwt = null;
    api.clearAuthHeader();
    toast.info("Logged out");
    goto("/login", { replaceState: true });
  }

  async function loadUser(): Promise<void> {
    if (typeof localStorage === "undefined") return;
    const stored = localStorage.getItem("phoenix_jwt");
    if (!stored) {
      user = null;
      return;
    }
    try {
      api.setAuthHeader(stored);
      const res = await authApi.me();
      user = res.user;
      jwt = stored;
    } catch (err) {
      user = null;
      jwt = null;
      api.clearAuthHeader();
    }
  }

  async function setup2FA(): Promise<{ secret: string; qr_url: string }> {
    return authApi.setup2FA();
  }

  async function enable2FA(otp: string): Promise<void> {
    await authApi.enable2FA(otp);
    if (user) {
      user.two_factor_enabled = true;
      user.totp_enabled = true;
    }
    toast.success("2FA enabled");
  }

  async function disable2FA(password: string): Promise<void> {
    await authApi.disable2FA(password);
    if (user) {
      user.two_factor_enabled = false;
      user.totp_enabled = false;
    }
    toast.success("2FA disabled");
  }

  if (initialJwt) {
    api.setAuthHeader(initialJwt);
  }

  return {
    get user() {
      return user;
    },
    get jwt() {
      return jwt;
    },
    get isAuthenticated() {
      return isAuthenticated;
    },
    login,
    verify2FA,
    passkeyLogin,
    completeOIDC,
    register,
    logout,
    loadUser,
    setup2FA,
    enable2FA,
    disable2FA,
  };
}

export const auth = createAuthStore();
