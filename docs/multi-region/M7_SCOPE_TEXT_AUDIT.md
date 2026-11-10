# M7 scope-text audit (PLAN §10, item 5)

Status: **code-level gaps closed; live acceptance open.** This refresh audits origin/main `8e14335` plus the Slack header fix in this change, taken 2026-11-09. The first audit (2026-11-01, `02354e1`) found four gaps. #178 and #181 closed gaps 1-3 and most of gap 4; this change closes the rest of gap 4. Live opt-in acceptance (an operator selects each mode on a real deployment and sees only the configured scopes) is still open and is not claimed here.

PLAN §10 item 5 says: "Explain regional versus aggregate scope in channel configuration and alert messages."

## What operators see now

| Surface | Regional delivery | Aggregate delivery | Source |
|---|---|---|---|
| Alert title (shared) | `<monitor> is DOWN` | `<monitor> is DOWN (aggregate)` | `alertTitle`, `aggregateTitleSuffix` in `internal/adapters/notifier/alert_format.go` (#178) |
| Plain-text body | message + `Region: <probe> (<location>)` | `AGGREGATE <monitor> (<policy>): <region>: <status>, …` | `alertBody`; a non-aggregate message can't start a line with `AGGREGATE ` (`disarmAggregatePrefix`) |
| Webhook JSON | `delivery_scope: "regional"`, `probe{…}`, `source_alert_id` | `delivery_scope: "aggregate"`, `source_alert_id`, no `probe` | `webhookEventPayload` (#178) |
| Custom templates | `{{ .alert.delivery_scope }}` | same | template context |
| Monitor probe panel | "Alerts are delivered per region." | "Saved delivery: one aggregate alert per monitor from the hub, instead of per-region alerts." `both`: "Saved delivery: per-region alerts and one aggregate alert per monitor from the hub." A saved-but-inactive mode adds "Aggregate paging is saved as desired and is not active. Regional alerts continue." | `ProbeAssignments.svelte`, `alert-delivery-text.ts` (#181) |
| Notification channel form | Scope hint: one channel gets every scope the monitor sends; tell them apart with `alert.delivery_scope` or the `AGGREGATE` prefix | same | `NotificationForm.svelte`, `notifications_delivery_scope_hint` (#181) |

## Provider check

Every provider's most visible title was rendered for one regional and one aggregate alert. Webhook-style providers were sent to a local test server and the raw request body was read. Telegram, LINE and SMTP post to fixed endpoints, so those were checked by reading the code.

| Provider | Visible title, regional → aggregate | How checked |
|---|---|---|
| Webhook | `title` `api is DOWN` → `api is DOWN (aggregate)`, plus `delivery_scope` | request body |
| Slack | header block `:x: api` → `:x: api (aggregate)` | request body; **was identical before this change** |
| Discord | embed title gains ` (aggregate)` | request body |
| Teams | `activityTitle`/`summary` gain ` (aggregate)` | request body |
| Mattermost | attachment title and text gain ` (aggregate)` | request body |
| Feishu | card header gains ` (aggregate)` | request body |
| Gotify | title gains ` (aggregate)` | request body |
| Bark | title gains ` (aggregate)` | request body |
| Telegram | first line uses `aggregateTitleSuffix` | code |
| LINE | first line uses `alertTitle` | code |
| SMTP | subject uses `alertTitleWithPrefix` | code |

## Gap status

1. Probe panel names the mode in effect, and pending versus active: **closed** (#181).
2. Channel configuration explains scope and how to tell scopes apart: **closed** (#181).
3. Aggregate webhook payload carries `delivery_scope: "aggregate"` and `source_alert_id`: **closed** (#178).
4. The two scopes have different titles: **closed** for the shared title (#178). Slack built its header block from the monitor name alone, so in `both` mode the two Slack pages still looked the same. This change adds the suffix there (`TestSlackSender_Send_AggregateHeaderNamesScope`).

## Still open

- Live opt-in acceptance: an operator turns on `PROBES_AGGREGATE_PAGING_ENABLED`, selects each mode, and receives only the configured scopes on a real channel. That needs a multi-region deployment.
- The admin UI has no mode selector yet (PLAN §10 item 1). Modes are set through the assignment API.

Items 1-4 in PLAN §10 cover the backend. [M7_OPERATIVE_CUTOVER.md](M7_OPERATIVE_CUTOVER.md) records it as merged and default-disabled. See [IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md).
