/**
 * Health / liveness API.
 */
import { api } from "./client";

export interface HealthLive {
  status?: string;
  /**
   * Server build version (e.g. "0.4.1"). Optional on the wire — older
   * backends omit it, so callers must treat absence as "unknown" and hide
   * any version UI rather than render a placeholder.
   */
  version?: string;
}

export const healthApi = {
  async live(): Promise<HealthLive> {
    return api.get<HealthLive>("/health/live");
  },
};
