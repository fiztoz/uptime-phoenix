# M6: local replay-drain pre-measure (1,000 assignments, ~50 min partition)

Status: **local engineering evidence, not the M6 drain gate.** One box, loopback links, no WAN, CPU shared by hub, MariaDB and 10 probes. The Run F/G AWS measurement is still required. The M6 checkbox in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) §9 stays unticked.

Run 2026-11-09 23:55 → 2026-11-10 02:01 ICT at main `8e14335` (hub built `-tags noweb`, `MODE=all`). Zero cloud calls.

## Setup
- Hub: one process, MariaDB 11.8.3 in user space on loopback (2 GiB buffer pool), `DB_MAX_OPEN_CONNS=50`. Host 8 vCPU / 15 GiB.
- 10 native probes (`probe run`), each behind its own pausable TCP proxy to the hub. A loopback HTTP target always returns 200.
- 100 load monitors × 10 probes = 1,000 assignments at 60 s, plus 10 local-only monitors on the hub.
- Sampler every 15 s: committed seq per probe, max observation lag, dirty rollup/projection backlog per resolution, API latency (`/api/monitors`, `/api/probes`, `/api/health/ready`), hub and DB CPU/RSS, local-only heartbeat age.
- Timeline (ICT): warm-up 23:56:54, partition (proxies drop all probe↔hub traffic) 00:06:54, heal 01:19:32, stop 02:01.

**Box freezes.** The box was frozen several times (sampler gaps of 44–1,060 s; every process, including the probes, stopped). About 1,387 s of the partition window was frozen, so the probes ran partitioned for about **2,970 s (≈ 49.5 min)**, not the 60 min planned. A 1,060 s freeze also fell in the drain watch at 01:36–01:54. Samples right after a freeze show inflated lag/age and are excluded from the latency figures below.

## Results

| Metric | Value |
|---|---|
| Steady arrival (warm-up) | 1,032 events/min (1,000 assignments + 10 local) |
| Steady event lag p95 | 55 s (60 s interval + 15 s sampling) |
| Backlog queued on the edges at heal | **53,794 events** (5,300–5,450 per probe), hub committed 0 during the partition |
| Time until every queued event was committed by the hub | **61–76 s** after heal (seq 11,155 → 65,849; lag 4,442 s → 84 s) |
| Hub ingest during the burst | ≈ 43,000 events/min, peak 1-min window 46,322/min (≈ 42× arrival) |
| Event lag back to steady (≤ 60 s) | +91 s |
| 1m rollup dirty backlog | peak 51,988 buckets at +76 s → steady 1,010 at +331 s (5.5 min) |
| Overall-projection dirty backlog | peak 7,346 at +331 s → steady 110 at +548 s (9.1 min wall, ≈ 6.8 min excluding a 142 s freeze) |
| Hub CPU during drain | p50 43 %, max 78 % of one core |
| MariaDB CPU during drain | p50 35 %, max 98 % of one core |
| `/api/monitors` during drain | p50 3.1 ms, p95 11.1 ms, max 21.2 ms; all HTTP 200 |
| `/api/probes` during drain | p50 5.7 ms, p95 11.7 ms; max 73 ms only on the first sample after a freeze |
| Local-only monitors during partition and drain | kept heartbeating; age ≤ 60 s outside freezes |
| Hub RSS | ≤ 56 MiB throughout |

So in this rig the bottleneck after a long partition is the rollup/projection rebuild (minutes), not telemetry ingest (about a minute). Event ingest ran at ~42× the arrival rate.

## Integrity (read-only checks at heal and at drain end)
- All 10 probes: edge `last_created_seq` = edge `committed_seq` = hub `committed_seq` (8,576–8,683), 0 unacked on the edge.
- Hub receipts contiguous 1..committed for every stream; 0 rejected receipts; 0 `probe_telemetry_gaps` rows.
- 86,045 observations; **0 duplicate (monitor, probe, observed_at)**; observations per stream = distinct seq.
- Every minute the box was running has 1,010 observations. Partial or missing minutes coincide exactly with box freezes (the probes were frozen too), not with the partition.
- 0 rows in `alerts`, `incidents`, `probe_delivery_intents`, `notification_sent_history`: the replay paged nobody.
- T07 side evidence: 30 min into the partition, the monitor health API showed every probe `disconnected`, `reason: stale_evidence`, status `unknown` (kept with the off-repo rig evidence).

**No loss and no duplicates.**

## Findings
1. **Transient MariaDB deadlocks in the first ~60 s of the drain.** Between heal and +62 s the hub logged `Error 1213 Deadlock` on the scheduler lease refresh (×2) and the probe configuration refresh (×1), plus 6 `capture applied settings failed: internal error`, 5 `operative cutover claim: internal error` and 2 `aggregate standby: internal error`. All were retried; none recurred after the burst, and local monitors were not affected. Two more lease-refresh deadlocks appeared only right after box freezes (00:29, 01:36). Worth a look under Run F load; not filed (GitHub holds).
2. The rig's first-pass catch-up estimate (2,271 s) was an artefact: its expected backlog used the wall-clock partition length including freezes (74,976 vs the actual 53,794). The figures above use the backlog actually measured on the edges.

## Not covered
- Real WAN latency/loss, four separate hosts, ≥ 24 h backlog, production DB storage. Those stay in Run F/G.
- A 60 min partition at full running time (≈ 49.5 min achieved). Scaling the burst linearly, a full hour adds ~10k events, i.e. ~15 s more ingest.

## Rig
The rig scripts (proxy, sampler, verifier) and raw samples are kept off-repo with the run evidence. They use only the public probe and hub binaries, the REST API and read-only SQL.
