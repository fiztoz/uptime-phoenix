/** Collection names from services.BackupDocument, including version-2 probe metadata. */
export const backupCollections = [
  "proxies",
  "notification_templates",
  "notifications",
  "tags",
  "monitor_groups",
  "monitors",
  "monitor_tags",
  "monitor_notifications",
  "group_notifications",
  "status_pages",
  "status_page_monitors",
  "status_page_cnames",
  "incidents",
  "maintenance_windows",
  "maintenance_monitors",
  "probes",
  "monitor_probe_assignments",
  "status_page_subscription_channels",
] as const;

export type BackupCollection = (typeof backupCollections)[number];

export class BackupReviewError extends Error {
  constructor(public readonly code: "object" | "version" | "collection") {
    super(code);
  }
}

/** Review the envelope only. Record fields and references are validated by the import API. */
export function inspectBackupDocument(value: unknown) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new BackupReviewError("object");
  }
  const doc = value as Record<string, unknown>;
  // Import accepts an omitted/zero version as the current schema for hand-written docs.
  const version = doc.version == null || doc.version === 0 ? 2 : doc.version;
  if (version !== 1 && version !== 2) throw new BackupReviewError("version");
  const counts = {} as Record<BackupCollection, number>;
  for (const key of backupCollections) {
    const rows = doc[key];
    if (rows == null) {
      counts[key] = 0;
    } else if (
      Array.isArray(rows) &&
      rows.every(
        (row) => row !== null && typeof row === "object" && !Array.isArray(row),
      )
    ) {
      counts[key] = rows.length;
    } else {
      throw new BackupReviewError("collection");
    }
  }
  return {
    doc,
    version,
    counts,
    total: Object.values(counts).reduce((sum, count) => sum + count, 0),
  };
}
