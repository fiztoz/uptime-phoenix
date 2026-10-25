"""Offline regressions for the remote m8-stage-authorization evaluator."""
import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import m8_remote_gate_evaluate as remote  # noqa: E402
import test_m6_dual_engine_gate as fixtures  # noqa: E402


class RemoteGateEvaluateTest(unittest.TestCase):
    def test_complete_synthetic_stream_passes(self):
        summary = remote.evaluate(fixtures.stage_lines())
        self.assertEqual(summary["result"], "passed", summary)
        self.assertEqual(summary["missing_required"], [])
        self.assertEqual(summary["required"], 324)

    def test_missing_host_action_leaf_fails(self):
        omitted = sorted(fixtures.HOST_ACTION_LEAVES)[0]
        summary = remote.evaluate(fixtures.stage_lines(fixtures.stage_event_values(omit=(omitted,))))
        self.assertEqual(summary["result"], "failed")
        self.assertIn(omitted, summary["missing_required"])

    def test_skip_failure_and_malformed_lines_fail(self):
        base = fixtures.stage_lines()
        for extra in (json.dumps({"Action": "skip", "Package": fixtures.PACKAGE, "Test": "TestX/mariadb/y"}) + "\n",
                      json.dumps({"Action": "fail", "Package": fixtures.PACKAGE, "Test": "TestX"}) + "\n",
                      "not-json\n"):
            self.assertEqual(remote.evaluate(base + [extra])["result"], "failed", extra)

    def test_unterminated_line_fails(self):
        lines = fixtures.stage_lines()
        lines[-1] = lines[-1].rstrip("\n")
        self.assertEqual(remote.evaluate(lines)["result"], "failed")


if __name__ == "__main__":
    unittest.main()
