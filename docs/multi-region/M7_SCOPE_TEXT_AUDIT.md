# M7 scope-text audit (PLAN §10, item 5)

Status: **open.** This is a code and unit-level audit of origin/main `02354e1`, taken 2026-11-01. It records what operators see today. It is not release acceptance.

PLAN §10 item 5 says: "Explain regional versus aggregate scope in channel configuration and alert messages."

## What operators see today

| Surface | Regional delivery | Aggregate delivery | Source |
|---|---|---|---|
| Alert title | `<monitor> is DOWN` | `<monitor> is DOWN` (same) | `internal/adapters/notifier/alert_format.go` `alertTitle` |
| Plain-text body | message + `Region: <probe> (<location>)` | `AGGREGATE <monitor> (<policy>): <region>: <status>, …` | `alertBody` (lines 75-83); `internal/core/services/operative_delivery_service.go:142` |
| Webhook JSON | `delivery_scope: "regional"`, `probe{…}`, `source_alert_id` | **no `delivery_scope`, no `source_alert_id`** | `webhookEventPayload` (lines 149-152) |
| Custom templates | `{{ .alert.delivery_scope }}` available | available | template context |
| Monitor probe panel | "Alerts are delivered per region." | Still says "Alerts are delivered per region." If the mode is saved but not active, it also says "Aggregate paging is saved as desired and is not active. Regional alerts continue." | `web/src/lib/components/ProbeAssignments.svelte:260-267`, `web/messages/en.json:1036-1037` |
| Notification channel configuration | nothing about scope | nothing about scope | `web/src/routes/(admin)/notifications` |

To reproduce the alert text, put a throwaway `_test.go` in `internal/adapters/notifier` that calls `alertTitle`, `alertBody` and `webhookEventPayload` with `DeliveryScope` set to `regional` and then `aggregate`. Run it with `go test -run … -v`.

## Gaps before item 5 can be ticked

1. The probe panel shows the same per-region sentence for every mode. It should name the mode in effect: regional, aggregate or both, and pending versus active.
2. Channel configuration should say that one channel gets one alert per region in regional mode, one hub alert per monitor in aggregate mode, and both in `both` mode. It should also say how to tell them apart: the `delivery_scope` template variable and the `AGGREGATE` message prefix.
3. Aggregate webhook payloads should carry `delivery_scope: "aggregate"` and the aggregate `source_alert_id`, so receivers can route and dedupe without parsing the message text.
4. Both scopes use the same title. In `both` mode, a receiver that keys on the title can't tell the two pages apart.

Items 1-4 in PLAN §10 cover the backend. [M7_OPERATIVE_CUTOVER.md](M7_OPERATIVE_CUTOVER.md) records it as merged and default-disabled (`PROBES_AGGREGATE_PAGING_ENABLED`). Live opt-in and release acceptance are still open; see [IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md).
