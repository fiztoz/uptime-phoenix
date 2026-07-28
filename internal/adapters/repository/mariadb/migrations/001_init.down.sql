-- 001_init.down.sql — Drop all tables in reverse dependency order

DROP TABLE IF EXISTS notification_sent_history;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS tls_info;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS maintenance_window_monitors;
DROP TABLE IF EXISTS maintenance_windows;
DROP TABLE IF EXISTS monitor_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS status_page_monitors;
DROP TABLE IF EXISTS status_page_cnames;
DROP TABLE IF EXISTS status_pages;
DROP TABLE IF EXISTS monitor_notification;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS heartbeat_1d;
DROP TABLE IF EXISTS heartbeat_1h;
DROP TABLE IF EXISTS heartbeat_1m;
DROP TABLE IF EXISTS heartbeats;
DROP TABLE IF EXISTS monitors;
DROP TABLE IF EXISTS docker_hosts;
DROP TABLE IF EXISTS proxies;
DROP TABLE IF EXISTS users;
