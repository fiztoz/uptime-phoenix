-- Explicit downgrade discards standby history only, never runtime/provider state.
DROP TABLE aggregate_standby_intents;
DROP TABLE aggregate_standby_incidents;
DROP TABLE aggregate_standby_state;
