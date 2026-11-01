-- Hub-observed last contact for remote probes (issue #133).
--
-- last_contact_at is the hub database clock (Unix seconds, the same clock and
-- precision as lease_until) at which the hub owning the live connector session
-- last accepted an authenticated health frame from the probe. Probes send one
-- every 15 seconds while connected. The write is fenced by owner_id and
-- generation on a live lease and never moves the value backward, so a stale or
-- expired session cannot advance it and hub-side timers (lease renewal, the
-- watchdog) never touch it. Release and takeover keep the value, so it freezes
-- while a probe is disconnected and advances again on the next accepted frame.
--
-- NULL means no contact has been recorded since this migration; existing rows
-- are not backfilled because no earlier column held hub-observed evidence.
-- ADD COLUMN IF NOT EXISTS keeps the script idempotent (068 tail-heal idiom).
ALTER TABLE probe_sessions
    ADD COLUMN IF NOT EXISTS last_contact_at BIGINT NULL CHECK (last_contact_at IS NULL OR last_contact_at > 0);
