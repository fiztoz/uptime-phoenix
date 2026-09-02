-- Per-channel opt-in for the public acknowledgement link that DOWN
-- notifications carry. Default 0: operators turn it on per channel.
ALTER TABLE notifications ADD COLUMN include_ack_url INTEGER NOT NULL DEFAULT 0;
