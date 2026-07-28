/**
 * WebAuthn (passkey) API wrappers + browser-ceremony helpers.
 *
 * The backend returns/accepts the raw WebAuthn JSON the browser expects, but
 * the browser API uses ArrayBuffers where the JSON uses base64url strings, so
 * we translate at the boundary with the helpers below.
 */
import { api } from "./client";
import type { LoginResponse, User } from "./auth";

export interface Passkey {
  id: number;
  name: string;
  credential_id: string;
  transports: string[];
  created_at: string;
  last_used_at?: string | null;
}

interface BeginResponse {
  session_id: string;
  // publicKey options as returned by the server (base64url-encoded fields).
  publicKey: PublicKeyCredentialCreationOptionsJSON &
    PublicKeyCredentialRequestOptionsJSON;
}

// Minimal JSON shapes — only the fields we decode/encode are typed.
interface PublicKeyCredentialCreationOptionsJSON {
  challenge: string;
  user?: { id: string; name: string; displayName: string };
  excludeCredentials?: { id: string; type: string; transports?: string[] }[];
}
interface PublicKeyCredentialRequestOptionsJSON {
  challenge: string;
  allowCredentials?: { id: string; type: string; transports?: string[] }[];
}

/** Whether this browser supports the WebAuthn API at all. */
export function isWebAuthnSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.PublicKeyCredential !== "undefined"
  );
}

// --- base64url <-> ArrayBuffer helpers -----------------------------------

function base64urlToBuffer(value: string): ArrayBuffer {
  const pad = "=".repeat((4 - (value.length % 4)) % 4);
  const base64 = (value + pad).replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(base64);
  const buf = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
  return buf.buffer;
}

function bufferToBase64url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let str = "";
  for (const b of bytes) str += String.fromCharCode(b);
  return btoa(str).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// Decode the server's creation options JSON into the structure
// navigator.credentials.create() expects.
function decodeCreationOptions(
  opts: PublicKeyCredentialCreationOptionsJSON,
): PublicKeyCredentialCreationOptions {
  return {
    ...(opts as unknown as PublicKeyCredentialCreationOptions),
    challenge: base64urlToBuffer(opts.challenge),
    user: opts.user
      ? { ...opts.user, id: base64urlToBuffer(opts.user.id) }
      : (undefined as unknown as PublicKeyCredentialUserEntity),
    excludeCredentials: (opts.excludeCredentials || []).map((c) => ({
      ...c,
      id: base64urlToBuffer(c.id),
      type: "public-key" as PublicKeyCredentialType,
      transports: c.transports as AuthenticatorTransport[] | undefined,
    })),
  };
}

function decodeRequestOptions(
  opts: PublicKeyCredentialRequestOptionsJSON,
): PublicKeyCredentialRequestOptions {
  return {
    ...(opts as unknown as PublicKeyCredentialRequestOptions),
    challenge: base64urlToBuffer(opts.challenge),
    allowCredentials: (opts.allowCredentials || []).map((c) => ({
      ...c,
      id: base64urlToBuffer(c.id),
      type: "public-key" as PublicKeyCredentialType,
      transports: c.transports as AuthenticatorTransport[] | undefined,
    })),
  };
}

// Encode a registration PublicKeyCredential into the JSON the server parses.
function encodeRegistrationCredential(cred: PublicKeyCredential): unknown {
  const att = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: bufferToBase64url(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: bufferToBase64url(att.clientDataJSON),
      attestationObject: bufferToBase64url(att.attestationObject),
      transports: att.getTransports ? att.getTransports() : [],
    },
  };
}

// Encode an assertion PublicKeyCredential into the JSON the server parses.
function encodeAssertionCredential(cred: PublicKeyCredential): unknown {
  const asr = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: bufferToBase64url(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: bufferToBase64url(asr.clientDataJSON),
      authenticatorData: bufferToBase64url(asr.authenticatorData),
      signature: bufferToBase64url(asr.signature),
      userHandle: asr.userHandle
        ? bufferToBase64url(asr.userHandle)
        : undefined,
    },
  };
}

export const webauthnApi = {
  /** List the current user's registered passkeys. */
  async list(): Promise<Passkey[]> {
    return api.get<Passkey[]>("/auth/webauthn/credentials");
  },

  /** Remove a passkey by id. */
  async remove(id: number): Promise<void> {
    return api.del(`/auth/webauthn/credentials/${id}`);
  },

  /**
   * Run the full registration ceremony for the logged-in user:
   * begin → navigator.credentials.create() → finish.
   */
  async register(name: string): Promise<Passkey> {
    const begin = await api.post<BeginResponse>(
      "/auth/webauthn/register/begin",
    );
    const credential = (await navigator.credentials.create({
      publicKey: decodeCreationOptions(begin.publicKey),
    })) as PublicKeyCredential | null;
    if (!credential) throw new Error("Passkey registration was cancelled");
    return api.post<Passkey>("/auth/webauthn/register/finish", {
      session_id: begin.session_id,
      name,
      credential: encodeRegistrationCredential(credential),
    });
  },

  /**
   * Run the full passwordless login ceremony for a username:
   * begin → navigator.credentials.get() → finish. Returns the session.
   */
  async login(username: string): Promise<LoginResponse & { user: User }> {
    const begin = await api.post<BeginResponse>("/auth/webauthn/login/begin", {
      username,
    });
    const credential = (await navigator.credentials.get({
      publicKey: decodeRequestOptions(begin.publicKey),
    })) as PublicKeyCredential | null;
    if (!credential) throw new Error("Passkey sign-in was cancelled");
    return api.post("/auth/webauthn/login/finish", {
      username,
      session_id: begin.session_id,
      credential: encodeAssertionCredential(credential),
    });
  },
};
