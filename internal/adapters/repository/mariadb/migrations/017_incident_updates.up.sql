-- Incident timeline updates (F3.2)
CREATE TABLE incident_updates (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    incident_id     BIGINT NOT NULL,
    status_page_id  BIGINT NOT NULL,
    status          VARCHAR(30) NOT NULL,
    content         TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_incident_updates_status CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    FOREIGN KEY (incident_id) REFERENCES incidents(id) ON DELETE CASCADE,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    INDEX idx_incident_updates_incident_order (incident_id, created_at, id),
    INDEX idx_incident_updates_page_order (status_page_id, incident_id, created_at, id)
);
