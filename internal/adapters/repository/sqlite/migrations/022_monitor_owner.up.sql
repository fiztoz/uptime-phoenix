-- Informational service contact. Authorization continues to use monitors.user_id
-- as the creating Phoenix account; owner has no permission semantics.
ALTER TABLE monitors ADD COLUMN owner TEXT NOT NULL DEFAULT '';
