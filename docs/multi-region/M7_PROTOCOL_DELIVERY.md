# M7 protocol delivery compatibility

This bounded slice makes the probe config snapshot protocol carry the desired
alert delivery mode stored by [desired delivery persistence](M7_DESIRED_DELIVERY.md).
It does **not** complete M7, activate aggregate paging, or suppress regional
paging. Regional notification delivery remains unchanged and fully active. A
config receipt is still only a configuration-document receipt: it is **not** an
applied-mode receipt and never proves a source stopped regional paging.

## Wire contract

The config assignment object gains one optional field:

- `alert_delivery` is omitted entirely for the legacy regional mode, so existing
  regional documents stay byte-identical. Only `aggregate` and `both` are
  encoded.
- When the key is absent, decoders read the assignment as `regional`. When
  present, the value must be a JSON **string** of exactly `regional`,
  `aggregate`, or `both`; `null`, non-string values, the empty string, and any
  other value reject the whole snapshot. The field is deliberately not a
  required decode member, because absence is valid.
- Unknown assignment fields remain ignored exactly as before; this slice adds no
  new rejection for previously tolerated keys.
- The hub config source read resolves the mode from
  `monitor_probe_assignment_sets.alert_delivery` through
  `domain.CanonicalAlertDelivery`: an absent legacy value is regional, and any
  other stored value fails the read instead of being coerced to a mode the
  operator never chose. No migration is involved; the column is migration `076`.

## Capability advertisement

Probes advertise `paging.aggregate.v1` (`probe.AggregatePagingCapability`) in
their runtime capability list to declare they understand the optional field.
Advertised does not mean active: the capability is **informational until a later
activation barrier**. It does not enable aggregate paging, hold or suppress
regional notifications, or make a config ACK mean the mode was applied.

The capability is intentionally **not** part of the enrollment frame
`Capabilities`, which stays `snapshot.v1` only, and it is never required before a
snapshot is accepted.

## Explicitly out of scope

- No call to `EvaluateAggregatePaging`; the decision foundation stays unwired.
- No regional notification suppression of any kind.
- No UI selector and no HTTP assignment API change.
- No migration.

## Production changes

| File | Change |
|---|---|
| `internal/core/domain/probe_config_build.go` | `ProbeConfigAssignment.AlertDelivery` (empty is legacy regional) |
| `internal/adapters/repository/probe_config_source.go` | resolve the stored mode via `domain.CanonicalAlertDelivery`, fail closed on invalid values |
| `internal/adapters/probe/config_snapshot.go` | `ConfigAssignment.alert_delivery` optional wire field and explicit decode handling |
| `internal/adapters/probe/config_encoder.go` | encode only `aggregate`/`both`; omit regional and empty |
| `internal/adapters/probe/command_runtime.go` | `AggregatePagingCapability = "paging.aggregate.v1"` |
| `cmd/probe/runtime.go` | advertise the capability |
