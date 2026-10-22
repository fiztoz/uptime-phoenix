"""Negative controls for strict OpenSSH acceptance and offline fixture policy."""
from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import signal
import tempfile
import unittest
from unittest.mock import patch

from m8_binary_stage_host_sandbox import GateError
import m8_binary_stage_ssh_acceptance as audit
import m8_binary_stage_ssh_sandbox as sandbox


def events(repetitions=3):
    result = [{"Action": "start", "Package": audit.PACKAGE}]

    def add(action, name):
        result.append({"Action": action, "Package": audit.PACKAGE, "Test": name})

    for _ in range(repetitions):
        add("run", audit.TOP)
        add("run", audit.TOP + "/Fixture")
        add("pass", audit.TOP + "/Fixture")
        add("run", audit.TOP + "/ActualHelper")
        for arch in ("amd64", "arm64"):
            add("run", audit.TOP + "/ActualHelper/" + arch)
            add("pass", audit.TOP + "/ActualHelper/" + arch)
        add("pass", audit.TOP + "/ActualHelper")
        for name in audit.REQUIRED[4:]:
            add("run", name)
            add("pass", name)
        add("pass", audit.TOP)
    result.append({"Action": "pass", "Package": audit.PACKAGE})
    return result


def text(items):
    return "".join(json.dumps(event) + "\n" for event in items)


class CompletionTests(unittest.TestCase):
    def verdict(self, items, code=0):
        return audit.audit_events(text(items), 3, code)

    def test_exact_completion(self):
        result = self.verdict(events())
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["test_passes"], 27)
        self.assertEqual(result["required_leaf_passes"], 24)

    def test_every_required_run_and_pass(self):
        for name in audit.ALLOWED:
            for action in ("run", "pass"):
                with self.subTest(name=name, action=action):
                    values = events()
                    values.pop(next(i for i, e in enumerate(values) if e.get("Test") == name and e["Action"] == action))
                    self.assertFalse(self.verdict(values)["passed"])

    def test_no_skip_or_failure(self):
        for action in ("skip", "fail"):
            values = events()
            values[2]["Action"] = action
            self.assertFalse(self.verdict(values)["passed"])

    def test_process_completion_required(self):
        for code in (1, -9, None):
            self.assertFalse(self.verdict(events(), code)["passed"])

    def test_package_completion_required(self):
        values = events()
        self.assertFalse(self.verdict(values[:-1])["passed"])
        self.assertFalse(self.verdict(values + [values[-1]])["passed"])

    def test_wrong_package_case_or_selection(self):
        for field, value in (("Package", "another/package"), ("Test", audit.TOP.lower()), ("Test", audit.TOP + "/unknown")):
            values = events()
            values[2][field] = value
            self.assertFalse(self.verdict(values)["passed"])

    def test_parent_only_and_zero_selection(self):
        self.assertFalse(self.verdict([e for e in events() if e.get("Test") in (None, audit.TOP)])["passed"])
        self.assertFalse(self.verdict([events()[0], events()[-1]])["passed"])

    def test_unjoined_parent(self):
        values = events()
        a = next(i for i, e in enumerate(values) if e.get("Test") == audit.TOP + "/ActualHelper" and e["Action"] == "pass")
        values.insert(5, values.pop(a))
        self.assertFalse(self.verdict(values)["passed"])

    def test_repetitions_not_all_in_one_iteration(self):
        values = [events()[0], {"Action": "run", "Package": audit.PACKAGE, "Test": audit.TOP}]
        for _ in range(3):
            values += [e for e in events(1) if e.get("Test") not in (None, audit.TOP)]
        values += [{"Action": "pass", "Package": audit.PACKAGE, "Test": audit.TOP}]
        for _ in range(2):
            values += [{"Action": action, "Package": audit.PACKAGE, "Test": audit.TOP} for action in ("run", "pass")]
        values += [events()[-1]]
        self.assertFalse(self.verdict(values)["passed"])

    def test_abnormal_output(self):
        for output in ("panic: bad", "fatal error: bad", "WARNING: DATA RACE", "test timed out", "warning: no tests to run", "[no test files]"):
            values = events()
            values.insert(2, {"Action": "output", "Package": audit.PACKAGE, "Output": output})
            self.assertFalse(self.verdict(values)["passed"])

    def test_malformed_and_truncated(self):
        valid = text(events())
        for bad in ("", valid[:-1], valid + "{\n", valid + "[]\n", valid.replace('"Action": "run"', '"Action": null', 1)):
            self.assertFalse(audit.audit_events(bad, 3, 0)["passed"])

    def test_subset_not_acceptance(self):
        self.assertFalse(self.verdict(events(1))["passed"])
        self.assertFalse(self.verdict(events(2))["passed"])
        self.assertFalse(self.verdict(events(4))["passed"])


def inspected(runtime=True):
    return {"HostConfig": {"NetworkMode": "none", "Privileged": False, "Binds": None, "PidMode": "", "IpcMode": "private",
                           "CapDrop": ["ALL"], "CapAdd": list(sandbox.CAPABILITIES) if runtime else [],
                           "ReadonlyRootfs": runtime, "SecurityOpt": ["no-new-privileges"], "PidsLimit": 128,
                           "Memory": 1024 ** 3, "NanoCpus": 2_000_000_000,
                           "Tmpfs": {p: "rw" for p in (("/config", "/tmp", "/opt") if runtime else ("/config",))}},
            "Config": {"Labels": {"phoenix.m8.ssh": "task"}, "User": "0:0", "Entrypoint": ["/fixtures/test"]}, "Mounts": []}


class PolicyTests(unittest.TestCase):
    def test_exact_runtime_and_carrier(self):
        for runtime in (False, True):
            sandbox.check_isolation(inspected(runtime), "task", runtime)
            args = sandbox.create_arguments("task", "image", runtime, "arm64")
            self.assertEqual(args[:3], ["create", "--pull", "never"])
            self.assertIn("--network", args)
            self.assertEqual(args[args.index("--network") + 1], "none")
            self.assertNotIn("--privileged", args)
            self.assertNotIn("--mount", args)
            self.assertNotIn("--volume", args)
            self.assertEqual(args.count("--cap-add"), 4 if runtime else 0)

    def test_capability_prefix_only_normalization(self):
        value = inspected()
        value["HostConfig"]["CapAdd"] = ["CAP_" + cap for cap in sandbox.CAPABILITIES]
        sandbox.check_isolation(value, "task", True)

    def test_unsafe_daemon_facts_refused(self):
        for key, value in (("NetworkMode", "host"), ("Privileged", True), ("Binds", ["/:/host"]), ("PidMode", "host"),
                           ("IpcMode", "host"), ("CapDrop", []), ("CapAdd", ["SYS_ADMIN"]), ("ReadonlyRootfs", False),
                           ("SecurityOpt", []), ("PidsLimit", 0), ("Memory", 0), ("NanoCpus", 0), ("Tmpfs", {})):
            with self.subTest(key=key):
                data = inspected()
                data["HostConfig"][key] = value
                with self.assertRaises(GateError):
                    sandbox.check_isolation(data, "task", True)

    def test_host_mounts_and_wrong_entrypoint_refused(self):
        for mounts in ([{"Type": "bind"}], [{"Type": "volume"}]):
            data = inspected()
            data["Mounts"] = mounts
            with self.assertRaises(GateError):
                sandbox.check_isolation(data, "task", True)
        for key, value in (("Entrypoint", ["/init"]), ("User", "1000"), ("Labels", {})):
            data = inspected()
            data["Config"][key] = value
            with self.assertRaises(GateError):
                sandbox.check_isolation(data, "task", True)

    def test_fixture_faults_bind_original_command(self):
        for body in sandbox.fixture_scripts().values():
            self.assertIn(("[ \"$SSH_ORIGINAL_COMMAND\" = '" + sandbox.FIXED_COMMAND + "' ] || exit 97").encode(), body)
            self.assertIn(sandbox.FIXED_COMMAND.encode(), body)
            self.assertNotIn(b"eval", body)

    def test_pinned_image_contract(self):
        self.assertIsNotNone(sandbox.PINNED_IMAGE.fullmatch("docker.io/example/sshd@sha256:" + "a" * 64))
        self.assertIsNotNone(sandbox.PINNED_IMAGE.fullmatch("registry.example:5000/repo@sha256:" + "a" * 64))
        for value in ("image:latest", "image@sha256:abc", "https://image@sha256:" + "a" * 64, "image@sha256:" + "a" * 64 + "\n"):
            self.assertIsNone(sandbox.PINNED_IMAGE.fullmatch(value))

    def test_preapproval_and_remote_refusals_do_no_io(self):
        with tempfile.TemporaryDirectory() as directory:
            for allow, endpoint in ((False, "unix:///not-a-socket"), (True, "tcp://example.invalid:2375")):
                work = Path(directory) / ("allowed" if allow else "not-approved")
                args = ["--docker-endpoint", endpoint, "--fixture-image", "image@sha256:" + "a" * 64,
                        "--go", "/not-a-compiler", "--work-dir", str(work)]
                if allow:
                    args += ["--allow-local-docker"]
                previous = signal.getsignal(signal.SIGTERM)
                with patch.object(sandbox, "command") as command, contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(sandbox.main(args), 1)
                    command.assert_not_called()
                self.assertFalse(work.exists())
                self.assertIs(signal.getsignal(signal.SIGTERM), previous)


if __name__ == "__main__":
    unittest.main()
