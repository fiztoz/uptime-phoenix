"""Offline tests for the M6 load-window analyzer's observed-cadence completeness."""
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import m6_load_analyzer as analyzer  # noqa: E402

T1, T2, CFG, CAD = 1000.0, 2800.0, 20, 20.25
PAIR_FIELDS = {"monitor_id", "probe_id", "configured_interval_s", "observed_cadence_s", "cadence_gaps_used",
               "cadence_fallback", "seen", "missing", "expected", "completeness", "max_gap_s"}


def series(start=T1 + 7.0, cadence=CAD, end=T2, skip=()):
    out, t, i = [], start, 0
    while t < end:
        if i not in skip:
            out.append(t)
        t += cadence
        i += 1
    return out


class PairCompleteness(unittest.TestCase):
    def test_slow_cadence_is_complete(self):
        r = analyzer.pair_completeness(series(), T1, T2, CFG)
        self.assertAlmostEqual(r["observed_cadence_s"], CAD)
        self.assertEqual((r["missing"], r["completeness"]), (0, 1.0))
        self.assertFalse(r["cadence_fallback"])
        # the window / configured-interval formula called this a failure
        self.assertLess(r["seen"] / ((T2 - T1) / CFG), analyzer.COMPLETENESS_THRESHOLD)

    def test_window_phase_does_not_matter(self):
        for start in (T1 + 0.1, T1 + 10, T1 + 20.2):
            self.assertEqual(analyzer.pair_completeness(series(start=start), T1, T2, CFG)["completeness"], 1.0, start)

    def test_one_missing_heartbeat_counts(self):
        r = analyzer.pair_completeness(series(skip={40}), T1, T2, CFG)
        self.assertEqual(r["missing"], 1)
        self.assertAlmostEqual(r["observed_cadence_s"], CAD)
        self.assertLess(r["completeness"], analyzer.COMPLETENESS_THRESHOLD)

    def test_missing_at_window_edges_counts(self):
        s = series()
        self.assertEqual(analyzer.pair_completeness(s[1:], T1, T2, CFG)["missing"], 1)
        self.assertEqual(analyzer.pair_completeness(s[:-1], T1, T2, CFG)["missing"], 1)

    def test_outage_cannot_inflate_cadence(self):
        skip = set(range(20, 50)) | set(range(51, 88, 2))
        r = analyzer.pair_completeness(series(skip=skip), T1, T2, CFG)
        self.assertAlmostEqual(r["observed_cadence_s"], CAD)
        self.assertGreater(r["missing"], 45)
        self.assertLess(r["completeness"], 0.6)

    def test_too_few_gaps_fall_back_to_configured(self):
        r = analyzer.pair_completeness([T1 + 5, T1 + 25.25], T1, T2, CFG)
        self.assertTrue(r["cadence_fallback"])
        self.assertEqual((r["observed_cadence_s"], r["cadence_gaps_used"]), (CFG, 0))
        self.assertLess(r["completeness"], 0.05)

    def test_no_rows_is_all_missing(self):
        r = analyzer.pair_completeness([], T1, T2, CFG)
        self.assertEqual(r["seen"], 0)
        self.assertGreater(r["missing"], 80)
        self.assertEqual(r["completeness"], 0.0)


class Evidence(unittest.TestCase):
    def rows(self):
        out = [("m1", "p1", t, t + 0.4) for t in series()]
        out += [("m2", "p1", t, t + 0.6) for t in series(skip={3})]
        out += [("m3", "p2", t, t + 0.2) for t in series(cadence=20.1)]
        return out

    def test_per_pair_fields_and_aggregate(self):
        ev = analyzer.analyze(self.rows(), T1, T2, CFG, label="unit")
        c = ev["completeness"]
        self.assertEqual(len(c["per_pair"]), 3)
        for p in c["per_pair"]:
            self.assertEqual(set(p), PAIR_FIELDS)
            self.assertEqual(p["configured_interval_s"], CFG)
            self.assertEqual(p["expected"], p["seen"] + p["missing"])
        by = {(p["monitor_id"], p["probe_id"]): p for p in c["per_pair"]}
        self.assertAlmostEqual(by[("m1", "p1")]["observed_cadence_s"], CAD)
        self.assertAlmostEqual(by[("m3", "p2")]["observed_cadence_s"], 20.1)
        self.assertEqual(by[("m2", "p1")]["missing"], 1)
        self.assertGreater(by[("m1", "p1")]["cadence_gaps_used"], 80)
        self.assertEqual(c["pairs_below_threshold"], 1)
        self.assertFalse(c["pass"])
        self.assertEqual(c["min_completeness"], by[("m2", "p1")]["completeness"])
        self.assertIn("mean_completeness", c)
        self.assertEqual(c["configured_interval_s"], CFG)
        self.assertEqual(c["threshold"], 0.995)
        self.assertIn("legacy_configured_interval", c)
        self.assertEqual(ev["ingest_delay_s"]["max"], 0.6)

    def test_probe_filter_and_duplicates(self):
        rows = self.rows() + [("m1", "p1", series()[5], None)]
        ev = analyzer.analyze(rows, T1, T2, CFG, probes=["p1"])
        self.assertEqual(ev["completeness"]["pairs"], 2)
        self.assertEqual(ev["duplicate_monitor_probe_time"], 1)

    def test_cli_reads_tsv_and_writes_evidence(self):
        with tempfile.TemporaryDirectory() as d:
            src, out = os.path.join(d, "obs.tsv"), os.path.join(d, "ev.json")
            with open(src, "w") as f:
                f.write("monitor_id\tprobe_id\tobserved_at\treceived_at\n")
                for m, p, o, r in self.rows():
                    f.write(f"{m}\t{p}\t{o}\t{r}\n")
            with redirect_stderr(io.StringIO()):
                code = analyzer.main(["--observations", src, "--start", str(T1), "--end", str(T2),
                                      "--configured-interval", str(CFG), "--out", out])
            self.assertEqual(code, 1)  # one pair misses a heartbeat
            with open(out) as f:
                ev = json.load(f)
            self.assertEqual(ev["schema_version"], 1)
            self.assertEqual(len(ev["completeness"]["per_pair"]), 3)


if __name__ == "__main__":
    unittest.main()
