#!/usr/bin/env python3
"""Read-only recheck tooling for the #223 UI gaps (#194, #196, #198).

Each check compares two independent views of the same data and needs no
product code. Database reads are plain SELECTs printed by `sql`, so QA can run
them through whatever client the rig has (for example `docker exec ...
mariadb -B`) and pipe the tab-separated output back in.

  # 194: overall timeline segment widths match the API intervals over the window
  ui_recheck.py timeline --api rows.json --dom dom.json [--chart chart.json] [--monitor monitor.json]

    rows.json  : GET /api/monitors/<id>/heartbeats?hours=24&order=asc&limit=10000
                 (the full overall stream, not the 60 the strip asks for)
    dom.json   : {"now_ms": <capture time>, "width_px": <strip px>,
                  "lead": {"duration_ms", "weight", "width_px"} | null,
                  "segments": [{"status","start","duration_ms","weight","width_px"}]}
                 read from [data-testid=recent-check-timeline] and its children's
                 data-status / data-start / data-duration-ms / data-weight.
    chart.json : GET /api/monitors/<id>/heartbeats/chart?hours=24 (optional)
    monitor.json: GET /api/monitors/<id> (optional; its created_at)

  # 198: projected-transition baseline for the Insights outage counts
  ui_recheck.py sql baseline --from <ISO> --to <ISO> [--monitor ID ...]  | <client> > hist.tsv
  ui_recheck.py baseline --tsv hist.tsv --insights insights.json   (window from the response)
  ui_recheck.py baseline --tsv hist.tsv --from <ISO> --to <ISO>

  # 196: overall history projection lag and backlog (real column names)
  ui_recheck.py sql lag

Exit status: 0 = every check passed, 1 = a mismatch (listed on stdout),
2 = bad input.
"""
from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass
from datetime import datetime, timezone

DOWN_LIKE = {"down", "pending"}
# Status codes in monitor_health_history (domain.Status).
STATUS_NAMES = {0: "down", 1: "up", 2: "pending", 3: "maintenance", 4: "unknown"}


def parse_time(value: str) -> float:
    """ISO-8601 / RFC3339 (or MariaDB 'YYYY-MM-DD HH:MM:SS[.ffffff]', UTC) to epoch ms."""
    v = value.strip().replace(" ", "T", 1)
    if v.endswith("Z"):
        v = v[:-1] + "+00:00"
    dt = datetime.fromisoformat(v)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.timestamp() * 1000.0


def fmt_ms(ms: float) -> str:
    return datetime.fromtimestamp(ms / 1000, tz=timezone.utc).isoformat()


# ---------------------------------------------------------------- #194

@dataclass
class Row:
    status: str
    start_ms: float


def api_rows(rows: list[dict]) -> list[Row]:
    out = [Row(str(r["status"]), parse_time(r["time"])) for r in rows]
    out.sort(key=lambda r: r.start_ms)
    return out


def expected_durations(rows: list[Row], now_ms: float, axis_start: float) -> list[float]:
    """Each overall row runs from its start to the next row's start; the newest
    runs until now. Clipped to the axis. Independent of the web implementation."""
    out = []
    for i, r in enumerate(rows):
        end = rows[i + 1].start_ms if i + 1 < len(rows) else max(now_ms, r.start_ms)
        out.append(max(0.0, max(end, axis_start) - max(r.start_ms, axis_start)))
    return out


def check_timeline(api: list[dict], dom: dict, chart: dict | None, window_hours: float,
                   max_segments: int, tol_ms: float, tol_px: float, created_at: str | None = None) -> list[str]:
    problems: list[str] = []
    rows = api_rows(api)
    if created_at and rows and rows[0].start_ms < parse_time(created_at) - tol_ms:
        # #194: nothing was monitored before the monitor existed.
        problems.append(f"API starts at {fmt_ms(rows[0].start_ms)}, before the monitor was created ({created_at})")
    for a, b in zip(rows, rows[1:]):
        if b.start_ms <= a.start_ms:
            problems.append(f"API rows not strictly increasing at {fmt_ms(b.start_ms)}")
    for a, b in zip(rows, rows[1:]):
        if a.status == b.status:
            problems.append(f"API repeats status {a.status} at {fmt_ms(b.start_ms)} (uncoalesced boundary)")
    now = float(dom["now_ms"])
    segs = dom["segments"]
    truncated = len(segs) >= max_segments
    shown = rows[-max_segments:] if truncated else rows
    if len(segs) != len(shown):
        problems.append(f"UI shows {len(segs)} segments, API has {len(shown)} in the shown range ({len(rows)} total)")
        return problems
    end = max(now, shown[-1].start_ms) if shown else now
    window_start = end - window_hours * 3_600_000
    axis_start = max(shown[0].start_ms, window_start) if truncated and shown else window_start
    total = end - axis_start
    want = expected_durations(shown, now, axis_start)
    lead_ms = max(0.0, min(shown[0].start_ms, end) - axis_start) if shown else 0.0
    width_px = float(dom.get("width_px") or 0)
    lead = dom.get("lead") or {"duration_ms": 0, "weight": 0, "width_px": 0}
    if abs(float(lead["duration_ms"]) - lead_ms) > tol_ms:
        problems.append(f"not-monitored lead {lead['duration_ms']} ms, expected {lead_ms:.0f} ms")
    weight_sum = float(lead["weight"])
    for i, (seg, row, dur) in enumerate(zip(segs, shown, want)):
        label = f"segment {i} ({row.status} @ {fmt_ms(row.start_ms)})"
        if seg["status"] != row.status or abs(parse_time(seg["start"]) - row.start_ms) > 1:
            problems.append(f"{label}: UI has {seg['status']} @ {seg['start']}")
            continue
        # The newest segment keeps growing between the UI render and capture.
        slack = tol_ms + (abs(now - float(dom.get("rendered_ms", now))) if i == len(segs) - 1 else 0)
        if abs(float(seg["duration_ms"]) - dur) > slack:
            problems.append(f"{label}: UI duration {seg['duration_ms']} ms, expected {dur:.0f} ms")
        if total > 0 and abs(float(seg["weight"]) - dur / total) > max(tol_ms / total, 1e-6) + (slack / total):
            problems.append(f"{label}: UI weight {seg['weight']}, expected {dur / total:.6f}")
        if width_px and "width_px" in seg and total > 0:
            if abs(float(seg["width_px"]) - width_px * dur / total) > tol_px + width_px * slack / total:
                problems.append(f"{label}: {seg['width_px']} px, expected {width_px * dur / total:.1f} px")
        weight_sum += float(seg["weight"])
    if segs and abs(weight_sum - 1.0) > 1e-3:
        problems.append(f"lead + segment weights sum to {weight_sum:.4f}, not 1")
    if chart is not None:
        problems += check_chart_runs(rows, now, chart, tol_ms)
    return problems


def runs(rows: list[Row], now_ms: float, pred) -> list[tuple[float, float]]:
    out: list[tuple[float, float]] = []
    for i, r in enumerate(rows):
        end = rows[i + 1].start_ms if i + 1 < len(rows) else now_ms
        if not pred(r.status):
            continue
        if out and out[-1][1] == r.start_ms:
            out[-1] = (out[-1][0], end)
        else:
            out.append((r.start_ms, end))
    return out


def check_chart_runs(rows: list[Row], now_ms: float, chart: dict, tol_ms: float) -> list[str]:
    """The chart endpoint merges the same reconstruction into down/pending and
    unknown runs. Both views must agree (the open run's end is 'now')."""
    problems = []
    for key, pred in (("downtime_intervals", lambda s: s in DOWN_LIKE), ("unknown_intervals", lambda s: s == "unknown")):
        want = runs(rows, now_ms, pred)
        got = [(parse_time(iv["start"]), parse_time(iv["end"])) for iv in chart.get(key, [])]
        if len(got) != len(want):
            problems.append(f"chart {key}: {len(got)} runs, rows give {len(want)}")
            continue
        for n, ((gs, ge), (ws, we)) in enumerate(zip(got, want)):
            last = n == len(want) - 1
            # RFC3339 in the chart view is second precision.
            if abs(gs - ws) > 1000 + tol_ms or (not last and abs(ge - we) > 1000 + tol_ms):
                problems.append(f"chart {key}[{n}]: {fmt_ms(gs)}..{fmt_ms(ge)} vs rows {fmt_ms(ws)}..{fmt_ms(we)}")
    return problems


# ---------------------------------------------------------------- #198

def history_rows(tsv: str) -> dict[int, list[tuple[float, float | None, str]]]:
    """TSV from `sql baseline`: monitor_id, started_at, ended_at|NULL, status."""
    out: dict[int, list] = {}
    for line in tsv.splitlines():
        parts = line.rstrip("\n").split("\t")
        if not parts or not parts[0].strip().isdigit():
            continue  # header
        mid, start, end, status = int(parts[0]), parts[1], parts[2], int(parts[3])
        out.setdefault(mid, []).append((parse_time(start), None if end in ("", "NULL") else parse_time(end), STATUS_NAMES.get(status, "unknown")))
    for v in out.values():
        v.sort(key=lambda r: (r[0], r[1] or float("inf")))
    return out


def baseline(rows: list[tuple[float, float | None, str]], from_ms: float, to_ms: float) -> dict:
    """Outage = a contiguous run of DOWN/PENDING intervals that reaches DOWN and
    began inside the window. 'Contiguous' means the next interval starts no
    later than the previous one ended; an UP, UNKNOWN or MAINTENANCE interval,
    or a hole, ends the run. This restates #198's expected rule independently of
    the Insights code."""
    outages, transitions = [], []
    downtime = 0.0
    run_start = None
    run_counted = False
    prev_end = None
    prev_status = None
    for start, end, status in rows:
        end_c = to_ms if end is None or end > to_ms else end
        if max(start, from_ms) < end_c and status == "down":
            downtime += end_c - max(start, from_ms)
        if status in DOWN_LIKE:
            if run_start is None or (prev_end is not None and start > prev_end):
                run_start, run_counted = start, False
            if status == "down" and not run_counted:
                run_counted = True
                if from_ms <= run_start < to_ms:
                    outages.append(fmt_ms(run_start))
        else:
            run_start = None
        if prev_status is not None and status != prev_status and from_ms <= start < to_ms:
            transitions.append({"at": fmt_ms(start), "from": prev_status, "to": status})
        prev_status = status
        e = end if end is not None else to_ms
        prev_end = e if prev_end is None or e > prev_end else prev_end
    return {"outage_count": len(outages), "outage_starts": outages,
            "downtime_seconds": int(downtime // 1000), "transitions": transitions}


def check_insights(base: dict[int, dict], insights: dict) -> tuple[list[str], list[str]]:
    """Compare GET /api/insights rows with the baseline. Returns (problems, notes)."""
    problems: list[str] = []
    seen = set()
    items = insights.get("rows", [])
    for item in items:
        mid = int(item["monitor_id"])
        if mid not in base:
            continue
        seen.add(mid)
        if int(item.get("outage_count", -1)) != base[mid]["outage_count"]:
            problems.append(f"monitor {mid}: Insights outage_count {item.get('outage_count')}, baseline {base[mid]['outage_count']} ({base[mid]['outage_starts']})")
        if abs(int(item.get("downtime_seconds", 0)) - base[mid]["downtime_seconds"]) > 60:
            problems.append(f"monitor {mid}: Insights downtime {item['downtime_seconds']} s, baseline {base[mid]['downtime_seconds']} s")
    # The response can be filtered or ranked, so a missing monitor is a note.
    notes = [f"monitor {mid}: in the baseline but not in the Insights response" for mid in sorted(set(base) - seen)]
    return problems, notes


# ---------------------------------------------------------------- SQL

def sql_baseline(from_iso: str, to_iso: str, monitors: list[int]) -> str:
    f = datetime.fromtimestamp(parse_time(from_iso) / 1000, tz=timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")
    t = datetime.fromtimestamp(parse_time(to_iso) / 1000, tz=timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")
    where = f" AND monitor_id IN ({','.join(str(int(m)) for m in monitors)})" if monitors else ""
    # Include the interval open at the window start (it decides whether a run
    # began inside the window).
    return ("SELECT monitor_id, started_at, ended_at, status FROM monitor_health_history "
            f"WHERE started_at < '{t}' AND (ended_at IS NULL OR ended_at > '{f}'){where} "
            "ORDER BY monitor_id, started_at, id;")


SQL_LAG = """-- #196 projection lag. probe_dirty_buckets holds the work the history
-- projector has not done yet, so the oldest queued bucket is the lag. Times UTC.
SELECT resolution, COUNT(*) AS queued, MIN(bucket) AS oldest,
       TIMESTAMPDIFF(SECOND, MIN(bucket), UTC_TIMESTAMP(6)) AS oldest_age_s
FROM probe_dirty_buckets GROUP BY resolution ORDER BY resolution;
-- #196 the most-behind monitors on the overall timeline
SELECT monitor_id, COUNT(*) AS queued, MIN(bucket) AS oldest,
       TIMESTAMPDIFF(SECOND, MIN(bucket), UTC_TIMESTAMP(6)) AS lag_s
FROM probe_dirty_buckets WHERE resolution = 'overall'
GROUP BY monitor_id ORDER BY lag_s DESC LIMIT 20;
-- #196 history head per monitor. An open row (ended_at NULL) is the current
-- state, so an old head with open_rows = 1 is not lag by itself.
SELECT monitor_id, MAX(COALESCE(ended_at, started_at)) AS head, SUM(ended_at IS NULL) AS open_rows,
       TIMESTAMPDIFF(SECOND, MAX(COALESCE(ended_at, started_at)), UTC_TIMESTAMP(6)) AS head_age_s
FROM monitor_health_history GROUP BY monitor_id ORDER BY head_age_s DESC LIMIT 20;"""


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    t = sub.add_parser("timeline", help="#194 strip widths vs API intervals")
    t.add_argument("--api", required=True)
    t.add_argument("--dom", required=True)
    t.add_argument("--chart")
    t.add_argument("--monitor", help="GET /api/monitors/<id> response (its created_at bounds the stream)")
    t.add_argument("--window-hours", type=float, default=24)
    t.add_argument("--max-segments", type=int, default=60)
    t.add_argument("--tol-ms", type=float, default=1000)
    t.add_argument("--tol-px", type=float, default=1.5)
    b = sub.add_parser("baseline", help="#198 outage baseline from monitor_health_history")
    b.add_argument("--tsv", required=True)
    b.add_argument("--from", dest="from_", help="window start (default: the Insights response's from)")
    b.add_argument("--to", help="window end (default: the Insights response's to)")
    b.add_argument("--insights", help="GET /api/insights?period=... response to compare")
    s = sub.add_parser("sql", help="print read-only SQL")
    s.add_argument("which", choices=["lag", "baseline"])
    s.add_argument("--from", dest="from_")
    s.add_argument("--to")
    s.add_argument("--monitor", type=int, action="append", default=[])
    a = p.parse_args(argv)
    try:
        if a.cmd == "sql":
            if a.which == "lag":
                print(SQL_LAG)
            else:
                if not a.from_ or not a.to:
                    p.error("sql baseline needs --from and --to")
                print(sql_baseline(a.from_, a.to, a.monitor))
            return 0
        if a.cmd == "timeline":
            api = json.load(open(a.api))
            dom = json.load(open(a.dom))
            chart = json.load(open(a.chart)) if a.chart else None
            monitor = json.load(open(a.monitor)) if a.monitor else {}
            monitor = monitor.get("monitor", monitor)
            problems = check_timeline(api, dom, chart, a.window_hours, a.max_segments, a.tol_ms, a.tol_px,
                                      monitor.get("created_at"))
            print(json.dumps({"segments": len(dom["segments"]), "api_rows": len(api), "problems": problems}, indent=2))
            return 1 if problems else 0
        hist = history_rows(open(a.tsv).read())
        insights = json.load(open(a.insights)) if a.insights else None
        from_iso = a.from_ or (insights or {}).get("from")
        to_iso = a.to or (insights or {}).get("to")
        if not from_iso or not to_iso:
            p.error("baseline needs --from/--to or an --insights response carrying them")
        f, t_ = parse_time(from_iso), parse_time(to_iso)
        base = {mid: baseline(rows, f, t_) for mid, rows in sorted(hist.items())}
        problems, notes = check_insights(base, insights) if insights is not None else ([], [])
        print(json.dumps({"window": [fmt_ms(f), fmt_ms(t_)], "baseline": {str(k): v for k, v in base.items()},
                          "problems": problems, "notes": notes}, indent=2))
        return 1 if problems else 0
    except (OSError, ValueError, KeyError, TypeError) as e:
        print(f"bad input: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
