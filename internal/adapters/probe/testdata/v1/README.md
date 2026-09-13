# Implemented M0 transport fixtures

This directory covers the currently implemented **framing and observation telemetry slice**. It does not claim complete V1 protocol support.

`envelope-hello.json` validates common framing only. The envelope decoder recognizes all message names documented in PROTOCOL.md, but does not validate their payloads. Typed decoders currently cover `telemetry.batch` containing only `observation` events, plus `telemetry.ack`, `telemetry.retry`, and `telemetry.gap`. Other event payloads return an explicit unsupported error. No fixture enables remote execution or creates a transport endpoint.

Golden files exercise valid messages and representative invalid contracts. Tests additionally construct maximum frame/event/batch sizes, missing/null fields, duplicate object keys at every depth, counter overflow, unknown optional fields, invalid status combinations, sequence discontinuities and collection limits. The maximum-sequence fixture demonstrates exact signed-64-bit preservation and contiguous observations sharing a timestamp.

Raw condition observations remain separate from evaluated/promoted condition state snapshots, whose schema and fixtures are deferred. Empty gap monitor lists mean unknown affected coverage. Expired TLS certificates retain negative days remaining.

Decoding proves syntax and bounded structure only. Services must still authorize probe/stream/assignment identity, validate current configuration and time bounds, enforce connection generations, compare cursor continuity, and commit durable receipts before acknowledging evidence. ACK decoding does not prove a commit occurred.

Deferred M0 fixtures include handshake payloads, configuration/state transfers, evaluated conditions, alerts/delivery/watchdog events, commands and enrollment, HTTP views, and browser contracts. These must be implemented before advertising a complete protocol capability.
