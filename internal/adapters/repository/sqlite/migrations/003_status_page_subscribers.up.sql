-- Status page webhook subscribers.
-- Created_at: 2026-06-27
-- Updated_at: 2026-06-27

CREATE TABLE status_page_subscribers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id  INTEGER NOT NULL,
    url             TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1,
    secret          TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

CREATE INDEX idx_sps_page ON status_page_subscribers (status_page_id);
