ALTER TABLE notification_templates
    ADD COLUMN config TEXT NOT NULL DEFAULT '{}';
