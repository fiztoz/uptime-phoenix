-- Reverse 014: drop email subscription tables and restore the legacy
-- webhook subscriber table under the original name. Legacy rows are
-- preserved. Email subscriber rows created after 014 are discarded.

DROP TABLE IF EXISTS status_page_subscription_channels;
DROP TABLE IF EXISTS status_page_subscribers;
ALTER TABLE status_page_subscribers_legacy_webhook RENAME TO status_page_subscribers;
