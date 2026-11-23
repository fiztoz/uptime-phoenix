#!/usr/bin/env python3
"""Offline M6 load-window analyzer: heartbeat completeness against the observed cadence.

It reads an exported table of probe observations and writes one evidence JSON
for a measurement window. The ">= 99.5 % heartbeat completeness" threshold is
evaluated with the expected interval set to the OBSERVED check cadence of each
monitor/probe pair; the configured interval is recorded alongside, together
with the legacy ratio (window / configured interval) for comparison.

Cadence rule, per (monitor, probe) pair inside [start, end):
- duplicate observed_at rows are counted once (and reported per pair), so a
  repeated row can never raise completeness.
- cadence = median of the gaps that look like one interval
  (gap <= SINGLE_INTERVAL_MAX x configured). Gaps caused by missed checks
  or outages are left out, so they cannot stretch the cadence however many
  there are. With fewer than MIN_CADENCE_GAPS such gaps the configured
  interval is used and the pair is marked cadence_fallback=true.
- the cadence is capped at CADENCE_CAP x configured (cadence_capped=true);
  a capped pair is also charged for the time it lost, i.e. interior missing
  is at least round((last - first) / cadence) - gaps. A probe more than 10 %
  slower than configured therefore cannot score 100 %.
- missing slots: an interior gap g misses round(g / cadence) - 1 slots, and
  at least one when it was too long to feed the cadence
  (g > SINGLE_INTERVAL_MAX x configured), so every gap either sets the
  cadence or is counted against it; the window edges (start -> first row,
  last row -> end) miss floor(edge / (cadence x (1 + EDGE_JITTER))) slots.
- expected = seen + missing; completeness = seen / expected.
A single missed heartbeat always counts as missing, and a perfect cadence
scores 100 % wherever the window happens to start.

Input: tab-separated rows "monitor_id probe_id observed_at received_at" with
epoch seconds (fractional allowed); see scripts/M6_LOAD_ANALYZER.md for the
export query. Standard library only.
"""
import argparse
import json
import statistics
import sys

SINGLE_INTERVAL_MAX = 1.5
CADENCE_CAP = 1.1
EDGE_JITTER = 0.05
MIN_CADENCE_GAPS = 3
COMPLETENESS_THRESHOLD = 0.995
SCHEMA_VERSION = 2


def pct(values, p):
    if not values:
        return None
    v = sorted(values)
    k = (len(v) - 1) * p / 100
    f = int(k)
    c = min(f + 1, len(v) - 1)
    return round(v[f] + (v[c] - v[f]) * (k - f), 3)


def gap_missing(g, cadence, configured):
    """Slots missed inside one interior gap; a gap too long to feed the cadence misses at least one."""
    slots = max(0, round(g / cadence) - 1)
    if g > SINGLE_INTERVAL_MAX * configured:
        slots = max(1, slots)
    return slots


def pair_completeness(times, start, end, configured):
    """Completeness of one monitor/probe pair; times are epoch seconds inside [start, end)."""
    ts = sorted(set(times))
    duplicates = len(times) - len(ts)
    gaps = [b - a for a, b in zip(ts, ts[1:])]
    single = [g for g in gaps if g <= SINGLE_INTERVAL_MAX * configured]
    fallback = len(single) < MIN_CADENCE_GAPS
    median = None if fallback else statistics.median(single)
    capped = median is not None and median > CADENCE_CAP * configured
    cadence = float(configured) if fallback else min(median, CADENCE_CAP * configured)
    edge_slot = cadence * (1 + EDGE_JITTER)
    if ts:
        missing = sum(gap_missing(g, cadence, configured) for g in gaps)
        if capped:
            missing = max(missing, round((ts[-1] - ts[0]) / cadence) - len(gaps))
        for edge in (ts[0] - start, end - ts[-1]):
            missing += int(max(0.0, edge) // edge_slot)
    else:
        missing = int((end - start) // edge_slot)
    expected = len(ts) + missing
    return {
        "configured_interval_s": configured,
        "observed_cadence_s": round(cadence, 3),
        "median_gap_s": None if median is None else round(median, 3),
        "cadence_gaps_used": 0 if fallback else len(single),
        "cadence_fallback": fallback,
        "cadence_capped": capped,
        "seen": len(ts),
        "duplicates": duplicates,
        "missing": missing,
        "expected": expected,
        "completeness": round(len(ts) / expected, 6) if expected else 1.0,
        "max_gap_s": round(max(gaps), 3) if gaps else None,
    }


def completeness_summary(pairs, start, end, configured, threshold=COMPLETENESS_THRESHOLD):
    """pairs: {(monitor_id, probe_id): [epoch, ...]} -> aggregate plus per-pair evidence."""
    per_pair = []
    for (monitor_id, probe_id), times in sorted(pairs.items()):
        row = {"monitor_id": monitor_id, "probe_id": probe_id}
        row.update(pair_completeness(times, start, end, configured))
        per_pair.append(row)
    out = {
        "basis": "expected interval = observed per-pair cadence (median single-interval gap, capped at cadence_cap x configured); duplicate rows counted once; configured interval recorded alongside",
        "configured_interval_s": configured,
        "threshold": threshold,
        "rule": {"single_interval_max": SINGLE_INTERVAL_MAX, "cadence_cap": CADENCE_CAP, "edge_jitter": EDGE_JITTER,
                 "min_cadence_gaps": MIN_CADENCE_GAPS},
        "pairs": len(per_pair),
    }
    if not per_pair:
        out.update({"pass": False, "per_pair": []})
        return out
    ratios = [p["completeness"] for p in per_pair]
    cadences = [p["observed_cadence_s"] for p in per_pair]
    legacy_expected = (end - start) / configured
    legacy = [p["seen"] / legacy_expected for p in per_pair]
    out.update({
        "observed_cadence_s": {"median": round(statistics.median(cadences), 3), "min": min(cadences), "max": max(cadences)},
        "pairs_cadence_fallback": sum(1 for p in per_pair if p["cadence_fallback"]),
        "pairs_cadence_capped": sum(1 for p in per_pair if p["cadence_capped"]),
        "seen": sum(p["seen"] for p in per_pair),
        "duplicates": sum(p["duplicates"] for p in per_pair),
        "missing": sum(p["missing"] for p in per_pair),
        "min_completeness": min(ratios),
        "mean_completeness": round(statistics.mean(ratios), 6),
        "pairs_below_threshold": sum(1 for r in ratios if r < threshold),
        "pass": all(r >= threshold for r in ratios),
        "legacy_configured_interval": {"expected_per_pair": round(legacy_expected, 3),
                                       "min_ratio": round(min(legacy), 6), "mean_ratio": round(statistics.mean(legacy), 6)},
        "per_pair": per_pair,
    })
    return out


def read_observations(lines):
    """Parse "monitor_id<TAB>probe_id<TAB>observed_at<TAB>received_at" rows (header and blanks ignored)."""
    rows = []
    for n, line in enumerate(lines, 1):
        parts = line.rstrip("\n").split("\t")
        if not line.strip() or parts[0] in ("monitor_id", "#"):
            continue
        if len(parts) < 3:
            raise ValueError(f"line {n}: expected at least 3 tab-separated fields")
        received = float(parts[3]) if len(parts) > 3 and parts[3] not in ("", "NULL") else None
        rows.append((parts[0], parts[1], float(parts[2]), received))
    return rows


def analyze(rows, start, end, configured, probes=None, label=""):
    keep = set(probes) if probes else None
    rows = [r for r in rows if keep is None or r[1] in keep]
    in_window = [r for r in rows if start <= r[2] < end]
    pairs = {}
    seen_keys = {}
    for monitor_id, probe_id, observed, _ in in_window:
        pairs.setdefault((monitor_id, probe_id), []).append(observed)
        seen_keys[(monitor_id, probe_id, observed)] = seen_keys.get((monitor_id, probe_id, observed), 0) + 1
    completeness = completeness_summary(pairs, start, end, configured)
    delays = [r[3] - r[2] for r in rows if r[3] is not None and start <= r[3] < end]
    gaps = [p["max_gap_s"] for p in completeness["per_pair"] if p["max_gap_s"] is not None]
    return {
        "schema_version": SCHEMA_VERSION,
        "label": label,
        "window_utc": [start, end],
        "window_s": end - start,
        "configured_interval_s": configured,
        "completeness": completeness,
        "ingest_delay_s": {"basis": "received_at - observed_at for rows received inside the window",
                           "n": len(delays), "p50": pct(delays, 50), "p95": pct(delays, 95), "p99": pct(delays, 99),
                           "max": round(max(delays), 3) if delays else None},
        "max_gap_s": max(gaps) if gaps else None,
        "pairs_with_gap_gt_2x_configured": sum(1 for g in gaps if g > 2 * configured),
        "duplicate_monitor_probe_time": sum(1 for v in seen_keys.values() if v > 1),
    }


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--observations", required=True, help="TSV export of probe observations ('-' for stdin)")
    ap.add_argument("--start", type=float, required=True, help="window start, epoch seconds UTC")
    ap.add_argument("--end", type=float, required=True, help="window end, epoch seconds UTC")
    ap.add_argument("--configured-interval", type=int, required=True, help="configured monitor check interval, seconds")
    ap.add_argument("--probes", default="", help="comma-separated probe IDs to include (default: all)")
    ap.add_argument("--label", default="")
    ap.add_argument("--out", default="-", help="evidence JSON path ('-' for stdout)")
    a = ap.parse_args(argv)
    if a.end <= a.start or a.configured_interval <= 0:
        ap.error("need --end > --start and --configured-interval > 0")
    src = sys.stdin if a.observations == "-" else open(a.observations, encoding="utf-8")
    with src:
        rows = read_observations(src)
    probes = [p for p in a.probes.split(",") if p] or None
    result = analyze(rows, a.start, a.end, a.configured_interval, probes, a.label)
    text = json.dumps(result, indent=1, sort_keys=True) + "\n"
    if a.out == "-":
        sys.stdout.write(text)
    else:
        with open(a.out, "w", encoding="utf-8") as f:
            f.write(text)
    c = result["completeness"]
    print(f"completeness pass={c['pass']} pairs={c['pairs']} min={c.get('min_completeness')} "
          f"cadence_median={c.get('observed_cadence_s', {}).get('median')} configured={a.configured_interval}", file=sys.stderr)
    return 0 if c["pass"] else 1


if __name__ == "__main__":
    sys.exit(main())
