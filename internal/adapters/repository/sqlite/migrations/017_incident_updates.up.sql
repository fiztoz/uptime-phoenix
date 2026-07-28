-- Incident timeline updates (F3.2)
CREATE TABLE incident_updates (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_id     INTEGER NOT NULL,
    status_page_id  INTEGER NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    content         TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (incident_id) REFERENCES incidents(id) ON DELETE CASCADE,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

CREATE INDEX idx_incident_updates_incident_order ON incident_updates (incident_id, created_at, id);
CREATE INDEX idx_incident_updates_page_order ON incident_updates (status_page_id, incident_id, created_at, id);
