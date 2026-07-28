-- Status page webhook subscribers.
-- Created_at: 2026-06-27
-- Updated_at: 2026-06-27

CREATE TABLE status_page_subscribers (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    url             VARCHAR(2048) NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    secret          VARCHAR(255),            -- HMAC signing secret (optional)
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    INDEX idx_sps_page (status_page_id)
);
