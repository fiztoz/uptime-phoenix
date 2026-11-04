# Issue #213 — AWS aggregate restart verification

**Verdict: partial fix verified; #213 remains open.** Queue reconciliation and
later-monitor progress passed. Incident continuity across a restart-driven drain
failed: one uninterrupted outage produced two aggregate incidents and two DOWN
pages, followed by an UP for only the replacement incident.

## Provenance and scope

- Date: 2026-11-04; campaign reference: `3f96c1a82c1d`.
- Clean source: `c55190d3a5149631c503ec40971dc6ecb98b8609`, merged PR #214.
- One actual AWS EC2 hub in `us-east-1`, with MariaDB 11 and the in-process bus.
- Four autonomous EC2 probes in `us-east-1`, `ap-southeast-1`,
  `ap-northeast-1`, and `eu-west-1`; not local containers simulating regions.
- Official Ubuntu 24.04 ARM64 images. Clocks reported synchronized; uploaded
  production app/probe binaries were hash-verified on every host.
- Protected configurations, pinned TLS enrollment, real connector sessions and
  source execution. Both monitors used all four probes with `any_down` policy.
- Recording webhook and non-forwarding SMTP sinks; no production data or real
  notification recipients. No manual database-state mutations.

Binary SHA-256 digests:

| Binary | SHA-256 |
|---|---|
| Hub (`cmd/app`) | `52cccecb581cb1dbd65f014ae0d58ceb2688f379df64443a01799d8a37ff8527` |
| Probe (`cmd/probe`) | `dabe189ad967262a0113d512dcc34496c622efa1bd4d5d9db6baac5e064d350d` |

The [public issue comment](https://github.com/fiztoz/uptime-phoenix/issues/213#issuecomment-6636228940)
records the results. The external campaign driver, private operational metadata
and raw evidence are not committed and are not a fresh-checkout CI gate.

## Executed scenario

1. Enroll all four probes and require all four to report UP.
2. Move monitor A to aggregate delivery and wait for its all-source barrier to
   become ACTIVE. Its SMTP and webhook channels are real recording transports.
3. Return HTTP 503 from Singapore's local target while the other three stay UP.
4. When the aggregate DOWN webhook reaches the receiver, SIGKILL the hub container
   before acknowledging the request. Capture the unfinished durable grant.
5. Restart the same container with the same database, installation key and source
   identities. Keep the target DOWN while observing queue and barrier recovery.
6. Restore HTTP 200 and capture recovery before changing monitor B's desired mode.
7. Move the already-existing monitor B from regional to aggregate. Verify it reaches
   ACTIVE, then delivers DOWN and UP for a controlled Tokyo failure/recovery.

The first deployment attempt failed before hub startup: MariaDB's unprivileged
entrypoint could not read the root-only test initialization SQL. The harness file
ownership was corrected and only the unused disposable test DB was recreated.
The subsequent deployment enrolled all four probes. No application fix was made
for this infrastructure setup failure.

## Observed results

| Acceptance check | Observation | Result |
|---|---|---|
| No replay of original crashed delivery | Exactly one webhook DOWN for the original incident; delivery stayed at attempt 1 | Pass |
| No indefinitely leased delivery | Terminal `failed / provider_outcome_unknown`, lease cleared | Pass |
| Honest provider history | Crashed grant stayed `in_flight`, `finished_at = NULL` | Pass |
| No aggregate conflict loop | Zero aggregate-delivery conflict warnings observed | Pass |
| Independent later cutover | B reached ACTIVE and delivered aggregate DOWN and UP | Pass |
| Original incident gets normal recovery | Instead closed `admin_closed / drained`; no original-incident UP | **Fail** |
| One outage does not re-page via barrier replacement | A sent another DOWN under a new incident while the target was still DOWN | **Fail** |

Both webhook and SMTP recorded **DOWN → DOWN → UP** for A. The two DOWNs had
**different incident identities**. This is distinct from the same-delivery replay
fixed by #214. A did eventually send UP, but only for its replacement incident.

### UTC chronology

- **16:09:38:** original incident opened; SMTP DOWN arrived.
- **16:09:39:** original webhook DOWN arrived; hub killed during provider I/O.
- **16:10:24.511:** recorded evaluation entered
  `draining / cutover_evidence_incomplete` without changing assignment revision
  or outage transition version.
- **16:10:30.508:** original incident closed administratively as drained.
- **16:11:32 snapshot:** replacement barrier ACTIVE.
- **16:11:37.582:** replacement-incident DOWN during the same continuous outage.
- **16:11:54.756:** replacement-incident UP after target recovery.

## Remaining mechanism and controls

At the first recorded drain evaluation, two child source-session leases had
expired while their parent runtime-owner leases still had time remaining. Source
session and capability generations still matched. Later observations showed new
session generations and successful barrier re-entry.

The production path is:

- `operative_cutover_evidence.go`: a live session is required for remote capability
  evidence; matching generation alone is insufficient.
- `operative_cutover_lifecycle.go`: `activeTx` drains on incomplete all-source
  evidence before aggregate health/recovery evaluation.
- `operative_aggregate_delivery.go`: `supersedeAggregatePlansTx` closes an open
  incident administratively during drain; drain does not admit a health recovery.

There were no monitor/notification configuration changes between A's activation
and its recovery capture. B's later desired-mode change caused a separate,
legitimate `configuration_changed` drain afterward. Recovery transport itself
worked for both A's replacement incident and B. Original queue termination,
barrier re-entry, B's successful cutover and the absence of conflict warnings
rule out an indefinitely stuck aggregate delivery as the observed blocker.

The follow-up needs an explicit incident-continuity contract across restart-driven
authority loss. Do not bypass session, source-hold or final provider authorization
fences to make recovery pass. Do not fabricate a provider completion or treat an
administrative close as a health recovery.

## Evidence limits and cleanup

- One completed AWS campaign, not a repeated SIGKILL soak or general release gate.
- Restart followed a post-kill DB snapshot and a two-second wait. Exact original
  two-second process downtime was not reproduced or instrumented.
- Linux ARM64 only; not a cross-architecture campaign.
- The separate dual-engine automated regressions cover additional lost-write,
  provider-return, retry and competing-consumer boundaries. They are not the same
  as this cloud process-kill run.
- Private local evidence was retained under `tmp/issue213-aws-3f96c1a82c1d/`:
  `REPORT.md`, `timeline.jsonl`, `result.json`, boundary snapshots, bounded
  production warnings and `cleanup.json`. It is not attached to this document.
- All task-owned instances, EBS volumes, network interfaces, security groups and
  EC2 key pairs were verified removed across all four regions. Generated local
  SSH keys were deleted. Cleanup completed within approximately 32 minutes of
  plan creation, inside the approved two-hour window.
- No Elastic IP, IAM role, DNS record, external DB or cache was created. Exact AWS
  billing charges were not yet verified; the approved US$5 target was not a hard
  billing cap.
- The cloud verification made no application source changes and no production
  deployment. It does not establish that all of #213 is fixed.
