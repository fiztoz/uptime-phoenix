"""Non-Docker refusal/lifetime controls for the isolated Linux gate runner."""
import ast
import contextlib
import io
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import m8_binary_stage_host_sandbox as gate


class SandboxRunnerControls(unittest.TestCase):
    def invoke(self, arguments):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = gate.main(arguments)
        return code, json.loads(output.getvalue())

    def arguments(self, work, endpoint="unix:///no-m8-socket"):
        return ["--docker-endpoint", endpoint, "--go", sys.executable, "--work-dir", str(work)]

    def test_create_has_explicit_never_pull_policy(self):
        tree = ast.parse(Path(gate.__file__).read_text())
        assignments = [node.value for node in ast.walk(tree) if isinstance(node, ast.Assign)
                       and any(isinstance(target, ast.Name) and target.id == "argv_create" for target in node.targets)]
        self.assertEqual(len(assignments), 1)
        self.assertIsInstance(assignments[0], ast.List)
        prefix = [ast.literal_eval(value) for value in assignments[0].elts[:3]]
        self.assertEqual(prefix, ["create", "--pull", "never"])

    def test_complete_matrix_requires_all_eight(self):
        report = {"full_matrix": True, "scenarios": {name: {"passed": True} for name in gate.SCENARIOS}}
        gate.complete_matrix_verdict(report)
        self.assertTrue(report["passed"])
        report["scenarios"].pop("unsafe_root_owner")
        gate.complete_matrix_verdict(report)
        self.assertFalse(report["passed"])
        self.assertEqual(report["error"], "partial_matrix_not_acceptance")

    def test_diagnostic_subset_never_returns_acceptance(self):
        report = {"full_matrix": False, "scenarios": {"positive": {"passed": True}}}
        gate.complete_matrix_verdict(report)
        self.assertFalse(report["passed"])
        self.assertTrue(report["diagnostic_passed"])

    def test_unknown_scenario_never_completes_matrix(self):
        report = {"full_matrix": True, "scenarios": {name: {"passed": True} for name in gate.SCENARIOS}}
        report["scenarios"]["other"] = {"passed": True}
        gate.complete_matrix_verdict(report)
        self.assertFalse(report["passed"])

    def test_failed_scenario_never_completes_matrix(self):
        report = {"full_matrix": True, "scenarios": {name: {"passed": True} for name in gate.SCENARIOS}}
        report["scenarios"]["positive"]["passed"] = False
        gate.complete_matrix_verdict(report)
        self.assertFalse(report["passed"])
        self.assertFalse(report["diagnostic_passed"])

    def test_explicit_approval_precedes_all_work(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "absent"
            code, report = self.invoke(self.arguments(target))
            self.assertEqual(code, 1)
            self.assertEqual(report["error"], "docker_approval_required")
            self.assertFalse(target.exists())

    def test_remote_endpoint_never_contacts_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "absent"
            code, report = self.invoke(["--allow-local-docker"] + self.arguments(target, "tcp://not-a-local-engine:2375"))
            self.assertEqual(code, 1)
            self.assertEqual(report["error"], "local_unix_endpoint_required")
            self.assertFalse(target.exists())

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "requires Unix socket metadata")
    def test_existing_directory_is_never_overwritten(self):
        # Keep AF_UNIX below the kernel path bound even with a deep task TMPDIR.
        with tempfile.TemporaryDirectory(prefix="m8-host-socket-", dir="/tmp") as directory:
            root = Path(directory)
            work = root / "evidence"
            work.mkdir()
            retained = work / "result.json"
            retained.write_bytes(b"original evidence\n")
            endpoint = root / "socket"
            with socket.socket(socket.AF_UNIX) as sock:
                sock.bind(str(endpoint))
                code, report = self.invoke(["--allow-local-docker"] + self.arguments(work, "unix://" + str(endpoint)))
            self.assertEqual(code, 1)
            self.assertEqual(report["error"], "fresh_work_directory_required")
            self.assertEqual(retained.read_bytes(), b"original evidence\n")
            self.assertEqual(sorted(p.name for p in work.iterdir()), ["result.json"])

    def test_interruption_joins_the_started_child(self):
        actual_popen = gate.subprocess.Popen
        started = []

        class InterruptOnce:
            def __init__(self, *args, **kwargs):
                self.process = actual_popen(*args, **kwargs)
                self.interrupted = False
                started.append(self.process)

            def __getattr__(self, name):
                return getattr(self.process, name)

            def wait(self, *args, **kwargs):
                if not self.interrupted:
                    self.interrupted = True
                    raise KeyboardInterrupt()
                return self.process.wait(*args, **kwargs)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with mock.patch.object(gate.subprocess, "Popen", InterruptOnce):
                with self.assertRaises(KeyboardInterrupt):
                    gate.command([sys.executable, "-c", "import time;time.sleep(30)"],
                                 "interrupted", root, {"PATH": os.environ.get("PATH", "")}, root)
            self.assertEqual(len(started), 1)
            self.assertIsNotNone(started[0].poll())

    def test_timeout_joins_owned_child_and_records_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pidfile = root / "pid"
            program = "import os,time;open(" + repr(str(pidfile)) + ", 'w').write(str(os.getpid()));time.sleep(30)"
            with self.assertRaisesRegex(gate.GateError, "^command_timeout$"):
                gate.command([sys.executable, "-c", program], "child", root,
                             {"PATH": os.environ.get("PATH", "")}, root, timeout=1)
            record = json.loads((root / "child/result.json").read_text())
            self.assertTrue(record["timed_out"])
            self.assertNotEqual(record["exit"], 0)
            with self.assertRaises(ProcessLookupError):
                os.kill(int(pidfile.read_text()), 0)


if __name__ == "__main__":
    unittest.main()
