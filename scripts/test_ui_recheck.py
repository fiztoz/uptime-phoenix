import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import ui_recheck as u  # noqa: E402

H = 3_600_000
NOW = u.parse_time("2026-11-09T12:00:00Z")


def iso(ms):
    return u.fmt_ms(ms).replace("+00:00", "Z")


def dom_for(rows, now=NOW, width=1000.0, window_h=24, max_segments=60):
    """A correct strip for the given API rows (what the fixed #194 UI renders)."""
    starts = [u.parse_time(r["time"]) for r in rows][-max_segments:]
    shown = rows[-max_segments:]
    truncated = len(rows) >= max_segments
    end = max(now, starts[-1])
    axis = max(starts[0], end - window_h * H) if truncated else end - window_h * H
    total = end - axis
    durs = [max(0, max(starts[i + 1] if i + 1 < len(starts) else end, axis) - max(s, axis)) for i, s in enumerate(starts)]
    lead = max(0, min(starts[0], end) - axis)
    return {"now_ms": now, "width_px": width,
            "lead": {"duration_ms": lead, "weight": lead / total, "width_px": width * lead / total},
            "segments": [{"status": r["status"], "start": r["time"], "duration_ms": d, "weight": d / total, "width_px": width * d / total}
                         for r, d in zip(shown, durs)]}


class TimelineTest(unittest.TestCase):
    def setUp(self):
        # Monitor created 6h ago: up, down 30 min, unknown 10 min, up.
        self.rows = [
            {"status": "up", "time": iso(NOW - 6 * H)},
            {"status": "down", "time": iso(NOW - 3 * H)},
            {"status": "unknown", "time": iso(NOW - 2.5 * H)},
            {"status": "up", "time": iso(NOW - 2.5 * H + 600_000)},
        ]

    def check(self, dom, chart=None, rows=None):
        return u.check_timeline(rows or self.rows, dom, chart, 24, 60, 1000, 1.5)

    def test_correct_strip_passes(self):
        self.assertEqual(self.check(dom_for(self.rows)), [])

    def test_equal_width_strip_is_caught(self):
        dom = dom_for(self.rows)
        for seg in dom["segments"]:
            seg["weight"], seg["width_px"] = 0.25, 250.0
        dom["lead"] = None
        problems = self.check(dom)
        self.assertTrue(any("weight" in p for p in problems), problems)
        self.assertTrue(any("lead" in p for p in problems), problems)

    def test_pre_creation_unknown_is_caught(self):
        # The #194 bug: a leading UNKNOWN row from the window start.
        rows = [{"status": "unknown", "time": iso(NOW - 24 * H)}] + self.rows
        dom = dom_for(rows)
        self.assertEqual(self.check(dom, rows=rows), [])  # internally consistent...
        problems = self.check(dom_for(self.rows), rows=rows)  # ...but not what the API gave
        self.assertTrue(problems)
        created = iso(NOW - 6 * H)
        problems = u.check_timeline(rows, dom, None, 24, 60, 1000, 1.5, created)
        self.assertTrue(any("before the monitor was created" in p for p in problems), problems)
        self.assertEqual(u.check_timeline(self.rows, dom_for(self.rows), None, 24, 60, 1000, 1.5, created), [])

    def test_truncated_strip_uses_newest_rows(self):
        rows = [{"status": "up" if i % 2 else "down", "time": iso(NOW - (100 - i) * 60_000)} for i in range(100)]
        dom = dom_for(rows)
        self.assertEqual(len(dom["segments"]), 60)
        self.assertEqual(dom["lead"]["duration_ms"], 0)
        self.assertEqual(self.check(dom, rows=rows), [])

    def test_uncoalesced_api_rows_are_reported(self):
        rows = self.rows + [{"status": "up", "time": iso(NOW - H)}]
        self.assertTrue(any("uncoalesced" in p for p in self.check(dom_for(rows), rows=rows)))

    def test_chart_runs_agree(self):
        chart = {"downtime_intervals": [{"start": iso(NOW - 3 * H), "end": iso(NOW - 2.5 * H)}],
                 "unknown_intervals": [{"start": iso(NOW - 2.5 * H), "end": iso(NOW - 2.5 * H + 600_000)}]}
        self.assertEqual(self.check(dom_for(self.rows), chart), [])
        chart["downtime_intervals"] = []
        self.assertTrue(any("downtime_intervals" in p for p in self.check(dom_for(self.rows), chart)))

    def test_segment_count_mismatch(self):
        dom = dom_for(self.rows)
        dom["segments"].pop()
        self.assertTrue(any("segments" in p for p in self.check(dom)))


def tsv(rows):
    lines = ["monitor_id\tstarted_at\tended_at\tstatus"]
    for mid, s, e, st in rows:
        lines.append(f"{mid}\t{s}\t{e or 'NULL'}\t{st}")
    return "\n".join(lines) + "\n"


def minute(m):
    return u.fmt_ms(NOW - 24 * H + m * 60_000).replace("T", " ").replace("+00:00", "")


class BaselineTest(unittest.TestCase):
    F, T = NOW - 24 * H, NOW

    def base(self, rows):
        return u.baseline(u.history_rows(tsv(rows))[1], self.F, self.T)

    def test_one_continuous_outage_in_minute_buckets_counts_once(self):
        # The #198 case: 459 one-minute DOWN rows are one outage.
        rows = [(1, minute(m), minute(m + 1), 0) for m in range(100, 559)]
        b = self.base(rows)
        self.assertEqual(b["outage_count"], 1)
        self.assertEqual(b["downtime_seconds"], 459 * 60)
        self.assertEqual(b["transitions"], [])

    def test_pending_then_down_is_one_outage(self):
        b = self.base([(1, minute(0), minute(10), 1), (1, minute(10), minute(12), 2), (1, minute(12), minute(20), 0), (1, minute(20), None, 1)])
        self.assertEqual(b["outage_count"], 1)
        self.assertEqual(b["outage_starts"], [iso(self.F + 10 * 60_000).replace("Z", "+00:00")])
        self.assertEqual([t["to"] for t in b["transitions"]], ["pending", "down", "up"])

    def test_unknown_and_holes_split_runs(self):
        b = self.base([(1, minute(0), minute(5), 0), (1, minute(5), minute(6), 4), (1, minute(6), minute(9), 0),
                       (1, minute(20), minute(25), 0)])
        self.assertEqual(b["outage_count"], 3)

    def test_outage_open_at_window_start_is_not_new(self):
        b = self.base([(1, u.fmt_ms(self.F - H).replace("+00:00", ""), minute(30), 0), (1, minute(30), None, 1)])
        self.assertEqual(b["outage_count"], 0)
        self.assertEqual(b["downtime_seconds"], 30 * 60)

    def test_pending_only_run_is_not_an_outage(self):
        self.assertEqual(self.base([(1, minute(0), minute(5), 2), (1, minute(5), None, 1)])["outage_count"], 0)

    def test_insights_comparison(self):
        base = {1: {"outage_count": 1, "outage_starts": [], "downtime_seconds": 600}, 2: {"outage_count": 0, "outage_starts": [], "downtime_seconds": 0}}
        problems, notes = u.check_insights(base, {"rows": [{"monitor_id": 1, "outage_count": 459, "downtime_seconds": 600}]})
        self.assertEqual(len(problems), 1)
        self.assertIn("459", problems[0])
        self.assertEqual(len(notes), 1)
        self.assertEqual(u.check_insights(base, {"rows": [{"monitor_id": 1, "outage_count": 1, "downtime_seconds": 630}]})[0], [])


class CliTest(unittest.TestCase):
    def run_cli(self, *args):
        return subprocess.run([sys.executable, "-B", os.path.join(os.path.dirname(__file__), "ui_recheck.py"), *args],
                              capture_output=True, text=True)

    def test_sql_lag_uses_real_columns(self):
        out = self.run_cli("sql", "lag").stdout
        self.assertIn("COALESCE(ended_at, started_at)", out)
        self.assertIn("probe_dirty_buckets", out)
        self.assertIn("resolution = 'overall'", out)
        self.assertNotIn("bucket_start", out)

    def test_sql_baseline_is_read_only_and_bounded(self):
        out = self.run_cli("sql", "baseline", "--from", "2026-11-08T12:00:00Z", "--to", "2026-11-09T12:00:00Z", "--monitor", "10").stdout
        self.assertTrue(out.startswith("SELECT "))
        self.assertIn("monitor_id IN (10)", out)
        self.assertIn("ended_at > '2026-11-08 12:00:00.000000'", out)

    def test_exit_codes(self):
        with tempfile.TemporaryDirectory() as d:
            h = os.path.join(d, "h.tsv")
            with open(h, "w") as f:
                f.write(tsv([(1, minute(m), minute(m + 1), 0) for m in range(10)]))
            ins = os.path.join(d, "i.json")
            with open(ins, "w") as f:
                json.dump({"from": iso(NOW - 24 * H), "to": iso(NOW), "rows": [{"monitor_id": 1, "outage_count": 10, "downtime_seconds": 600}]}, f)
            r = self.run_cli("baseline", "--tsv", h, "--insights", ins)
            self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
            self.assertEqual(json.loads(r.stdout)["baseline"]["1"]["outage_count"], 1)
            self.assertEqual(self.run_cli("baseline", "--tsv", os.path.join(d, "missing.tsv"), "--from", "2026-11-09T00:00:00Z", "--to", "2026-11-09T01:00:00Z").returncode, 2)


if __name__ == "__main__":
    unittest.main()
