-- Status-page email subscriptions (Sprint C Track B).
-- Preserve dormant webhook subscriber rows under an explicit legacy name,
-- then create the real email-subscriber + channel tables under the
-- canonical names.
--
-- NOTE: SQLite keeps index names when a table is RENAME'd, so the new
-- email table must use distinct index names (idx_sps_email_*) rather than
-- reusing idx_sps_page from migration 003.

ALTER TABLE status_page_subscribers RENAME TO status_page_subscribers_legacy_webhook;

CREATE TABLE status_page_subscribers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id  INTEGER NOT NULL,
    email           TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 0,
    confirmed_at    TIMESTAMP NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    UNIQUE (status_page_id, email)
);

CREATE INDEX idx_sps_email_page ON status_page_subscribers (status_page_id);
CREATE INDEX idx_sps_email_page_active ON status_page_subscribers (status_page_id, active);

CREATE TABLE status_page_subscription_channels (
    status_page_id   INTEGER NOT NULL PRIMARY KEY,
    notification_id  INTEGER NOT NULL,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);
