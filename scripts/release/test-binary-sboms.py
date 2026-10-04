"""Exercise the standalone SBOM stage with retained binary artifacts."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class BinarySBOMStage(unittest.TestCase):
    def run_stage(self, mode="success", matching=True):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        tools = root / "tools"
        tools.mkdir()
        syft = tools / "syft"
        syft.write_text("#!/bin/sh\ncase \"$SBOM_TEST_MODE\" in\n"
                        "failure) printf partial; exit 7;;\n"
                        "empty) exit 0;;\n"
                        "*) printf '{\"spdxVersion\":\"SPDX-2.3\"}';;\nesac\n")
        syft.chmod(0o700)
        binaries = root / "out" / "binaries"
        binaries.mkdir(parents=True)
        (binaries / "uptime-phoenix_0.4.5_linux_arm64").write_bytes(b"old")
        if matching:
            for name in ("uptime-phoenix", "uptime-phoenix-worker"):
                (binaries / f"{name}_0.5.0-rc.1_linux_arm64").write_bytes(b"candidate")
        env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"],
                   VERSION="0.5.0-rc.1", OUT_DIR=str(root / "out"), STAGES="sbom",
                   SKIP_SBOM="0", SBOM_TEST_MODE=mode)
        result = subprocess.run(["/bin/bash", str(ROOT / "scripts/release/dry-run.sh")],
                                cwd=ROOT, env=env, capture_output=True, text=True, timeout=15)
        return result, root / "out" / "sbom"

    def test_existing_current_version_binaries_are_scanned(self):
        result, output = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(list(output.glob("*.spdx.json"))), 2)
        self.assertFalse(any("0.4.5" in p.name for p in output.iterdir()))

    def test_scanner_failure_rejects_partial_output(self):
        result, output = self.run_stage("failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(output.iterdir()), [])

    def test_empty_output_is_not_success(self):
        result, output = self.run_stage("empty")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(output.iterdir()), [])

    def test_missing_candidate_binaries_fail(self):
        result, _ = self.run_stage(matching=False)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
