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
PAIR_FIELDS = {"monitor_id", "probe_id", "configured_interval_s", "observed_cadence_s", "median_gap_s",
               "cadence_gaps_used", "cadence_fallback", "cadence_capped", "seen", "duplicates", "missing",
               "expected", "completeness", "max_gap_s"}


def series(start=T1 + 7.0, cadence=CAD, end=T2, skip=()):
    out, t, i = [], start, 0
    while t < end:
        if i not in skip:
            out.append(t)
        t += cadence
        i += 1
    return out


def from_gaps(gaps, first=T1 + 7.0):
    out = [first]
    for g in gaps:
        out.append(out[-1] + g)
    return out


def pc(times, start=T1, end=T2):
    return analyzer.pair_completeness(times, start, end, CFG)


def tight(times):
    """Window that ends 10 s after the last row, so only interior gaps can miss."""
    return analyzer.pair_completeness(times, T1, max(times) + 10.0, CFG)


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

    def test_gap_at_single_interval_max_is_cadence_and_just_over_is_a_miss(self):
        # 30 s == 1.5 x configured still looks like one interval; 30.01 s cannot feed the cadence, so it must miss
        at = tight(from_gaps([CAD] * 40 + [30.0] + [CAD] * 40))
        self.assertEqual((at["missing"], at["cadence_gaps_used"]), (0, 81))
        over = tight(from_gaps([CAD] * 40 + [30.01] + [CAD] * 40))
        self.assertEqual((over["missing"], over["cadence_gaps_used"]), (1, 80))

    def test_excluded_gap_is_never_free_when_cadence_is_above_configured(self):
        # regression: 31 s is too long to feed the median (> 30 s) but round(31 / 21.5) - 1 == 0
        r = tight(from_gaps([21.5] * 40 + [31.0] + [21.5] * 30))
        self.assertAlmostEqual(r["observed_cadence_s"], 21.5)
        self.assertFalse(r["cadence_capped"])
        self.assertEqual(r["missing"], 1)
        self.assertLess(r["completeness"], analyzer.COMPLETENESS_THRESHOLD)

    def test_inflated_median_with_35s_gap_is_not_complete(self):
        # regression (QA): many 24 s gaps and one 35 s gap scored 100 %
        r = tight(from_gaps([24.0] * 40 + [35.0] + [24.0] * 30))
        self.assertTrue(r["cadence_capped"])
        self.assertGreaterEqual(r["missing"], 1)
        self.assertLess(r["completeness"], analyzer.COMPLETENESS_THRESHOLD)

    def test_two_cadence_gaps_fall_back_three_use_median(self):
        two = pc(from_gaps([21.0, 21.0]))
        self.assertTrue(two["cadence_fallback"])
        self.assertEqual((two["observed_cadence_s"], two["median_gap_s"], two["cadence_gaps_used"]), (CFG, None, 0))
        three = pc(from_gaps([21.0, 21.0, 21.0]))
        self.assertFalse(three["cadence_fallback"])
        self.assertEqual((three["observed_cadence_s"], three["median_gap_s"], three["cadence_gaps_used"]), (21.0, 21.0, 3))

    def test_cadence_is_median_not_mean(self):
        r = tight(from_gaps([20.0] * 3 + [29.0] * 2))
        self.assertEqual(r["observed_cadence_s"], 20.0)

    def test_interior_rounding(self):
        # cadence 18 s (faster than configured): 1.6 x cadence misses one slot, 1.3 x misses none
        base = [18.0] * 20
        self.assertEqual(tight(from_gaps(base + [28.8] + base))["missing"], 1)
        self.assertEqual(tight(from_gaps(base + [23.4] + base))["missing"], 0)

    def test_extra_early_beat_is_not_negative_missing(self):
        s = series(cadence=20.0)
        r = pc(sorted(s + [s[10] + 5.0]))
        self.assertEqual((r["seen"], r["missing"]), (len(s) + 1, 0))

    def test_edge_factor_is_1_05(self):
        # cadence exactly 20 s -> an edge misses a slot from 21.0 s on, not before
        body = from_gaps([20.0] * 80, first=0.0)
        span = body[-1]
        for edge, want in ((20.99, 0), (21.0, 1), (22.0, 1), (42.0, 2)):
            lead = pc([t + edge for t in body], start=0.0, end=edge + span + 1.0)
            self.assertEqual(lead["missing"], want, ("leading", edge))
            trail = pc([t + 1.0 for t in body], start=0.0, end=1.0 + span + edge)
            self.assertEqual(trail["missing"], want, ("trailing", edge))

    def test_empty_pair_uses_edge_slot(self):
        self.assertEqual(pc([])["missing"], int((T2 - T1) // (CFG * 1.05)))

    def test_unsorted_input_is_sorted(self):
        s = series(skip={40})
        self.assertEqual(pc(list(reversed(s))), pc(s))

    def test_duplicates_are_counted_once(self):
        # regression (QA): 198 seen + 1 missing is 0.994975; a duplicated row made it 199 / 200 = 0.995 (pass)
        s = from_gaps([20.0] * 198, first=T1 + 5.0)
        del s[100]
        clean = pc(s, end=T1 + 3980.0)
        self.assertEqual((clean["seen"], clean["missing"], clean["duplicates"]), (198, 1, 0))
        dup = pc(s + [s[7]], end=T1 + 3980.0)
        self.assertEqual((dup["seen"], dup["missing"], dup["duplicates"]), (198, 1, 1))
        self.assertEqual(dup["completeness"], clean["completeness"])
        self.assertLess(dup["completeness"], analyzer.COMPLETENESS_THRESHOLD)


class CadenceCap(unittest.TestCase):
    def test_reported_slow_cadence_is_not_capped(self):
        r = pc(series())
        self.assertFalse(r["cadence_capped"])
        self.assertEqual((r["missing"], r["completeness"]), (0, 1.0))

    def test_just_under_cap_is_complete(self):
        r = pc(series(cadence=21.9))
        self.assertFalse(r["cadence_capped"])
        self.assertAlmostEqual(r["median_gap_s"], 21.9)
        self.assertEqual(r["completeness"], 1.0)

    def test_late_beats_under_the_cap_are_not_charged(self):
        # uncapped pairs are judged gap by gap; 25 s beats at a 20 s median are late, not missing
        r = tight(from_gaps([20.0] * 50 + [25.0] * 30))
        self.assertFalse(r["cadence_capped"])
        self.assertEqual((r["observed_cadence_s"], r["missing"]), (20.0, 0))

    def test_median_equal_to_cap_is_not_capped(self):
        r = pc(series(cadence=22.0))
        self.assertFalse(r["cadence_capped"])
        self.assertEqual(r["completeness"], 1.0)

    def test_over_cap_is_capped_and_charged(self):
        r = pc(series(cadence=23.0))
        self.assertTrue(r["cadence_capped"])
        self.assertEqual((r["observed_cadence_s"], r["median_gap_s"]), (22.0, 23.0))
        self.assertLess(r["completeness"], analyzer.COMPLETENESS_THRESHOLD)

    def test_uniformly_slow_probe_fails(self):
        for slow in (1.4, 1.5):
            r = pc(series(cadence=slow * CFG))
            self.assertTrue(r["cadence_capped"], slow)
            self.assertEqual(r["observed_cadence_s"], 22.0)
            # roughly the share of beats lost against 22 s
            self.assertLess(r["completeness"], 22.0 / (slow * CFG) + 0.02, slow)
            self.assertGreater(r["completeness"], 22.0 / (slow * CFG) - 0.02, slow)

    def test_slow_drift_fails(self):
        gaps = [20.0 + 9.0 * i / 80 for i in range(81)]
        r = pc(from_gaps(gaps))
        self.assertTrue(r["cadence_capped"])
        self.assertLess(r["completeness"], analyzer.COMPLETENESS_THRESHOLD)


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
        ratios = [p["completeness"] for p in c["per_pair"]]
        self.assertEqual(c["mean_completeness"], round(sum(ratios) / len(ratios), 6))  # unweighted
        self.assertEqual((c["pairs_cadence_capped"], c["duplicates"]), (0, 0))
        self.assertEqual(c["configured_interval_s"], CFG)
        self.assertEqual(c["threshold"], 0.995)
        self.assertIn("legacy_configured_interval", c)
        self.assertEqual(ev["ingest_delay_s"]["max"], 0.6)

    def test_probe_filter_and_duplicates(self):
        rows = self.rows() + [("m1", "p1", series()[5], None)]
        ev = analyzer.analyze(rows, T1, T2, CFG, probes=["p1"])
        self.assertEqual(ev["completeness"]["pairs"], 2)
        self.assertEqual(ev["duplicate_monitor_probe_time"], 1)
        by = {p["monitor_id"]: p for p in ev["completeness"]["per_pair"]}
        self.assertEqual((by["m1"]["duplicates"], by["m1"]["completeness"]), (1, 1.0))
        self.assertEqual(by["m1"]["seen"], len(series()))
        self.assertEqual(ev["completeness"]["duplicates"], 1)

    def test_capped_pairs_are_counted(self):
        rows = self.rows() + [("m4", "p3", t, None) for t in series(cadence=28.0)]
        c = analyzer.analyze(rows, T1, T2, CFG)["completeness"]
        self.assertEqual(c["pairs_cadence_capped"], 1)
        self.assertEqual(c["rule"]["cadence_cap"], 1.1)

    def test_duplicate_cannot_lift_a_failing_window(self):
        s = from_gaps([20.0] * 198, first=T1 + 5.0)
        del s[100]
        rows = [("m1", "p1", t, None) for t in s] + [("m1", "p1", s[7], None)]
        ev = analyzer.analyze(rows, T1, T1 + 3980.0, CFG)
        self.assertFalse(ev["completeness"]["pass"])
        self.assertEqual(ev["completeness"]["min_completeness"], round(198 / 199, 6))

    def test_window_end_is_exclusive(self):
        rows = self.rows()
        base = analyzer.analyze(rows, T1, T2, CFG)
        more = analyzer.analyze(rows + [("m1", "p1", T2, None)], T1, T2, CFG)
        self.assertEqual(more["completeness"]["seen"], base["completeness"]["seen"])

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
            self.assertEqual(ev["schema_version"], 2)
            self.assertEqual(len(ev["completeness"]["per_pair"]), 3)


if __name__ == "__main__":
    unittest.main()
