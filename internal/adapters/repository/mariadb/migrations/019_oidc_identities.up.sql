-- F5 Sprint 13: OIDC external identity links.
--
-- Linking key is the immutable (issuer, subject) pair from a validated ID
-- token. One Phoenix user may hold at most one identity per issuer.
-- See docs/F5-S13-OIDC-CONTRACTS.md.

CREATE TABLE oidc_identities (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    issuer        VARCHAR(512) NOT NULL,
    subject       VARCHAR(512) NOT NULL,
    email         VARCHAR(255) NOT NULL DEFAULT '',
    last_login_at TIMESTAMP NULL DEFAULT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    UNIQUE KEY uq_oidc_issuer_subject (issuer, subject),
    UNIQUE KEY uq_oidc_user_issuer (user_id, issuer),
    INDEX idx_oidc_identities_user (user_id)
);
