ALTER TABLE status_pages
    DROP COLUMN IF EXISTS show_powered_by,
    DROP COLUMN IF EXISTS favicon,
    MODIFY COLUMN icon VARCHAR(255) NULL;
