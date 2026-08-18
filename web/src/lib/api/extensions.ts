/**
 * Admin extension catalog. GET /api/extensions returns iframe plugins
 * listed by the server; an empty list is a valid (no-plugins) response.
 */
import { api } from "./client";

export interface Extension {
  id: string;
  title: string;
  path: string;
  /** Same-origin path the plugin image serves (default `{path}/icon.svg`). */
  icon: string;
}

/** Same-host path only — no scheme, protocol-relative URL, or `..`. */
export function isSafeExtensionIconPath(icon: string): boolean {
  return (
    icon.startsWith("/") &&
    !icon.startsWith("//") &&
    !icon.includes(":") &&
    !icon.includes("\\") &&
    !icon.includes("..")
  );
}

/** Plugin convention: `{path}/icon.svg` unless a safe `icon` override is set. */
export function extensionIconSrc(path: string, icon?: unknown): string {
  if (typeof icon === "string" && isSafeExtensionIconPath(icon.trim())) {
    return icon.trim();
  }
  return `${path.replace(/\/+$/, "")}/icon.svg`;
}

/** Keep id/title/path/icon only. Unknown fields and invalid rows are dropped. */
export function normalizeExtension(raw: unknown): Extension | null {
  if (raw == null || typeof raw !== "object" || Array.isArray(raw)) {
    return null;
  }
  const rec = raw as Record<string, unknown>;
  const { id, title, path } = rec;
  if (
    typeof id !== "string" ||
    !id ||
    typeof title !== "string" ||
    !title ||
    typeof path !== "string" ||
    !path
  ) {
    return null;
  }
  return { id, title, path, icon: extensionIconSrc(path, rec.icon) };
}

/** Normalize a list payload; non-arrays become []. */
export function normalizeExtensions(raw: unknown): Extension[] {
  if (!Array.isArray(raw)) return [];
  const out: Extension[] = [];
  for (const item of raw) {
    const ext = normalizeExtension(item);
    if (ext) out.push(ext);
  }
  return out;
}

export const extensionsApi = {
  async list(): Promise<Extension[]> {
    const data = await api.get<unknown>("/extensions");
    return normalizeExtensions(data);
  },
};
