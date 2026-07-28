/**
 * Backup export/import API wrappers.
 *
 * Export intentionally returns secrets (notification tokens, proxy passwords)
 * so a backup is fully restorable — see backend services.BackupDocument.
 */
import { api } from "./client";

export interface BackupDocument {
  version: number;
  exported_at: string;
  proxies: unknown[];
  notifications: unknown[];
  tags: unknown[];
  monitors: unknown[];
  monitor_tags: unknown[];
  monitor_notifications: unknown[];
  status_pages: unknown[];
  status_page_monitors: unknown[];
  status_page_cnames: unknown[];
  incidents: unknown[];
  maintenance_windows: unknown[];
  maintenance_monitors: unknown[];
}

export interface ImportSkipped {
  kind: string;
  id?: number;
  name?: string;
  reason: string;
}

export interface ImportSummary {
  proxies_created: number;
  notifications_created: number;
  tags_created: number;
  tags_reused: number;
  monitors_created: number;
  monitor_tags_created: number;
  monitor_notifications_created: number;
  status_pages_created: number;
  status_page_monitors_created: number;
  status_page_cnames_created: number;
  incidents_created: number;
  maintenance_windows_created: number;
  maintenance_monitors_created: number;
  skipped: ImportSkipped[];
}

export const backupApi = {
  /** Download the authenticated user's full configuration backup. */
  async export(): Promise<BackupDocument> {
    return api.get<BackupDocument>("/backup/export");
  },

  /** Merge-import a backup document (creates new entities; no overwrite). */
  async import(
    doc: BackupDocument | Record<string, unknown>,
  ): Promise<ImportSummary> {
    return api.post<ImportSummary>("/backup/import", doc);
  },
};
