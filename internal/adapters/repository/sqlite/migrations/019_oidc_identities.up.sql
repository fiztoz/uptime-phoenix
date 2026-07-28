-- F5 Sprint 13: OIDC external identity links.
--
-- Linking key is the immutable (issuer, subject) pair from a validated ID
-- token. One Phoenix user may hold at most one identity per issuer.
-- See docs/F5-S13-OIDC-CONTRACTS.md.

CREATE TABLE oidc_identities (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       INTEGER NOT NULL,
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    email         TEXT NOT NULL DEFAULT '',
    last_login_at DATETIME NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    UNIQUE (issuer, subject),
    UNIQUE (user_id, issuer)
);

CREATE INDEX idx_oidc_identities_user ON oidc_identities(user_id);
