-- F3.5 white-label: favicon URL/data-URL, remove-branding toggle, widen icon
-- for data-URL logos. See docs/F3.5-WHITE-LABEL-CONTRACTS.md.

ALTER TABLE status_pages
    MODIFY COLUMN icon TEXT NULL,
    ADD COLUMN favicon TEXT NULL AFTER icon,
    ADD COLUMN show_powered_by BOOLEAN NOT NULL DEFAULT TRUE AFTER auto_resolve_incidents;
