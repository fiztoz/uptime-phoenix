-- Per-channel opt-in for the public acknowledgement link that DOWN
-- notifications carry. Default false: operators turn it on per channel.
ALTER TABLE notifications
    ADD COLUMN include_ack_url BOOLEAN NOT NULL DEFAULT FALSE
        AFTER is_default;
