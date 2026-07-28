-- Status-page email subscriptions (Sprint C Track B).
-- Preserve dormant webhook subscriber rows under an explicit legacy name,
-- then create the real email-subscriber + channel tables under the
-- canonical names.

RENAME TABLE status_page_subscribers TO status_page_subscribers_legacy_webhook;

CREATE TABLE status_page_subscribers (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    email           VARCHAR(320) NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT FALSE,
    confirmed_at    TIMESTAMP NULL DEFAULT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    UNIQUE KEY uq_sps_page_email (status_page_id, email),
    INDEX idx_sps_page (status_page_id),
    INDEX idx_sps_page_active (status_page_id, active)
);

CREATE TABLE status_page_subscription_channels (
    status_page_id   BIGINT NOT NULL PRIMARY KEY,
    notification_id  BIGINT NOT NULL,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);
