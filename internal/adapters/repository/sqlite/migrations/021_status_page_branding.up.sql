-- F3.5 white-label: favicon + remove-branding. SQLite icon was already TEXT-capable
-- via affinity; add columns only. See docs/F3.5-WHITE-LABEL-CONTRACTS.md.

ALTER TABLE status_pages ADD COLUMN favicon TEXT NOT NULL DEFAULT '';
ALTER TABLE status_pages ADD COLUMN show_powered_by INTEGER NOT NULL DEFAULT 1;
