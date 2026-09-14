# Implemented M0 transport fixtures

This directory covers the currently implemented **framing, mixed telemetry, and current-state snapshot contracts**. It does not claim complete V1 protocol support.

`envelope-hello.json` validates common framing only. The envelope decoder recognizes all message names documented in PROTOCOL.md, but does not validate their payloads. Typed decoders cover `telemetry.batch` containing `observation`, `alert.transition`, `watchdog.transition`, `delivery.result`, and `condition.transition`, plus `telemetry.ack`, `telemetry.retry`, and `telemetry.gap`. Unknown event kinds return an explicit unsupported error. No fixture enables remote execution or creates a transport endpoint.

Golden files exercise valid messages and representative invalid contracts. Tests additionally construct maximum frame/event/batch sizes, missing/null fields, duplicate object keys at every depth, counter overflow, unknown optional fields, invalid status combinations, sequence discontinuities and collection limits. The maximum-sequence fixture demonstrates exact signed-64-bit preservation and contiguous observations sharing a timestamp.

Raw condition observations remain separate from evaluated/promoted condition state snapshots. Empty gap monitor lists mean unknown affected coverage. Expired TLS certificates retain negative days remaining.

The 41 `state-*` fixtures cover the complete snapshot schema and the four state transfer frames. `state-snapshot-*` files are reconstructed JSON documents, not envelopes. All other `state-*` files are envelopes routed to the matching typed decoder. The valid empty begin/chunk/commit fixtures form one hash-verified transfer of the exact bytes in `state-snapshot-empty.json`; its unknown optional member also demonstrates that hashing precedes typed reserialization. `state-applied-empty.json` is a receipt shape, not evidence that an application transaction ran.

Condition examples distinguish first-ever OK/warning/error, a warning after OK, confirmed warning, first/confirmed recovery, warning hysteresis, and stale source evidence. Maximum sequences retain distinct same-second observations. Invalid snapshots exercise missing promotion metadata, impossible candidates/counts, sequence bounds, duplicated assignments/observations/conditions, and invalid source status/incident identity.

Assembler tests additionally exercise out-of-order and identical duplicate chunks, conflicting retries, fixed-deadline expiry, cancellation, connection/stream/config fencing within a transfer, incomplete commits, exact byte totals, corrupt hashes, begin/content metadata conflicts, duplicate JSON keys after reconstruction, and terminal staging cleanup. Successful assembly returns typed evidence only.

The 46 incident/delivery/condition fixtures bring the total to 112. The mixed-lifecycle batch demonstrates independent monitor evidence, incident versions, acknowledgement and escalation cancellation, delivery retry/success against the original incident version, capacity promotion, certificate identity, and watchdog attribution. All `batch-*` files use the same complete batch decoder. Subject/lifecycle/nullability conflicts, unsupported scope claims, certificate thresholds, delivery outcome rules, and missing/overflow versions are rejected. Typed round trips do not forward unknown fields into persistence DTOs. Service authorization and immutable identity/version checks against durable records remain pending.

Decoding proves syntax and bounded structure only. Services must still authorize probe/stream/assignment identity, validate current configuration and time bounds, enforce connection generations, compare cursor continuity, and commit durable receipts before acknowledging evidence. ACK decoding does not prove a commit occurred.

Deferred M0 fixtures include handshake payloads, complete configuration transfers, commands and enrollment, HTTP views, and browser contracts. Durable projection application/receipts and authenticated assignment/incident/channel checks are also pending. These must be implemented before advertising a complete protocol capability.
