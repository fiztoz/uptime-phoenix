"""Negative controls for the fixed restricted-key matrix and selector binding."""
from __future__ import annotations

import json
import unittest

import m8_binary_stage_ssh_acceptance as sender
import m8_binary_stage_ssh_authorization_acceptance as authorization
import m8_binary_stage_ssh_sandbox as sandbox
from test_m8_binary_stage_ssh import events as sender_events


def events(repetitions=3):
    result = [{"Action": "start", "Package": sender.PACKAGE}]

    def add(action, name):
        result.append({"Action": action, "Package": sender.PACKAGE, "Test": name})

    for _ in range(repetitions):
        add("run", authorization.TOP)
        add("run", authorization.TOP + "/Fixture")
        add("pass", authorization.TOP + "/Fixture")
        add("run", authorization.TOP + "/Sender")
        for arch in ("amd64", "arm64"):
            add("run", authorization.TOP + "/Sender/" + arch)
            add("pass", authorization.TOP + "/Sender/" + arch)
        add("pass", authorization.TOP + "/Sender")
        for name in authorization.REQUIRED[4:]:
            add("run", name)
            add("pass", name)
        add("pass", authorization.TOP)
    result.append({"Action": "pass", "Package": sender.PACKAGE})
    return result


def text(values):
    return "".join(json.dumps(value) + "\n" for value in values)


class AuthorizationCompletionTests(unittest.TestCase):
    def verdict(self, values, code=0, repetitions=3):
        return authorization.audit_events(text(values), repetitions, code)

    def test_exact_completion(self):
        result = self.verdict(events())
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["test_passes"], 39)
        self.assertEqual(result["required_leaf_passes"], 36)
        self.assertEqual(result["package_passes"], 1)

    def test_each_required_run_and_pass(self):
        for name in authorization.ALLOWED:
            for action in ("run", "pass"):
                with self.subTest(name=name, action=action):
                    values = events()
                    values.pop(next(i for i, e in enumerate(values)
                                    if e.get("Test") == name and e["Action"] == action))
                    self.assertFalse(self.verdict(values)["passed"])

    def test_normal_process_exit_required(self):
        for code in (1, -9, None):
            self.assertFalse(self.verdict(events(), code)["passed"])

    def test_normal_package_completion_required(self):
        values = events()
        for bad in (values[:-1], values + [values[-1]],
                    values + [{"Action": "output", "Package": sender.PACKAGE, "Output": "late"}]):
            self.assertFalse(self.verdict(bad)["passed"])

    def test_skip_and_failure_never_pass(self):
        for action in ("skip", "fail"):
            values = events()
            values[2]["Action"] = action
            self.assertFalse(self.verdict(values)["passed"])

    def test_wrong_package_case_or_name(self):
        for field, value in (("Package", "wrong/package"), ("Test", authorization.TOP.lower()),
                             ("Test", authorization.TOP + "/Unknown")):
            values = events()
            values[2][field] = value
            self.assertFalse(self.verdict(values)["passed"])

    def test_parent_only_and_zero_selection(self):
        self.assertFalse(self.verdict([e for e in events() if e.get("Test") in (None, authorization.TOP)])["passed"])
        self.assertFalse(self.verdict([events()[0], events()[-1]])["passed"])

    def test_joined_parent_required(self):
        values = events()
        i = next(i for i, e in enumerate(values)
                 if e.get("Test") == authorization.TOP + "/Sender" and e["Action"] == "pass")
        values.insert(5, values.pop(i))
        self.assertFalse(self.verdict(values)["passed"])

    def test_child_cannot_run_before_immediate_parent(self):
        values = events()
        i = next(i for i, e in enumerate(values)
                 if e.get("Test") == authorization.TOP + "/Sender" and e["Action"] == "run")
        values.insert(i + 2, values.pop(i))
        result = self.verdict(values)
        self.assertFalse(result["passed"])
        self.assertIn("child_without_active_parent", result["errors"])

    def test_repetitions_cannot_be_bunched(self):
        values = [events()[0], {"Action": "run", "Package": sender.PACKAGE, "Test": authorization.TOP}]
        for _ in range(3):
            values += [e for e in events(1) if e.get("Test") not in (None, authorization.TOP)]
        values += [{"Action": "pass", "Package": sender.PACKAGE, "Test": authorization.TOP}]
        for _ in range(2):
            values += [{"Action": action, "Package": sender.PACKAGE, "Test": authorization.TOP}
                       for action in ("run", "pass")]
        values += [events()[-1]]
        self.assertFalse(self.verdict(values)["passed"])

    def test_subset_or_extra_repetition_not_acceptance(self):
        for n in (1, 2, 4):
            self.assertFalse(self.verdict(events(n))["passed"])
        self.assertFalse(self.verdict(events(), repetitions=0)["passed"])

    def test_malformed_truncated_and_abnormal(self):
        valid = text(events())
        for bad in ("", valid[:-1], valid + "{\n", valid + "[]\n"):
            self.assertFalse(authorization.audit_events(bad, 3, 0)["passed"])
        for output in ("panic: bad", "fatal error: bad", "WARNING: DATA RACE", "test timed out",
                       "warning: no tests to run", "[no test files]"):
            values = events()
            values.insert(2, {"Action": "output", "Package": sender.PACKAGE, "Output": output})
            self.assertFalse(self.verdict(values)["passed"])

    def test_contracts_cannot_substitute_for_each_other(self):
        self.assertFalse(authorization.audit_events(text(sender_events()), 3, 0)["passed"])
        self.assertFalse(sender.audit_events(text(events()), 3, 0)["passed"])
        self.assertTrue(sender.audit_events(text(sender_events()), 3, 0)["passed"])

    def test_invalid_contract_refused(self):
        top = authorization.TOP
        for required, allowed in (((), ()), ((top,), (top, top)),
                                  ((top + "/x",), (top, top + "/x")),
                                  ((top,), (top, top + "/Missing/Child"))):
            result = sender.audit_named_events(text(events()), 3, 0, top=top,
                                              required=required, allowed=allowed)
            self.assertFalse(result["passed"])
            self.assertIn("invalid_named_contract", result["errors"])


class AuthorizationSelectorTests(unittest.TestCase):
    def test_fixed_contract_binding(self):
        top, audit, scope = sandbox.acceptance_contract(True)
        self.assertEqual(top, authorization.TOP)
        self.assertIs(audit, authorization.audit_events)
        self.assertEqual(scope, "actual-linux-openssh-restricted-key")
        top, audit, scope = sandbox.acceptance_contract(False)
        self.assertEqual(top, sender.TOP)
        self.assertIs(audit, sender.audit_events)
        self.assertEqual(scope, "actual-linux-openssh-binary-stage")

    def test_runtime_requires_matching_top_and_extra_optin(self):
        args = sandbox.create_arguments("owned", "cached", True, "arm64", True)
        self.assertIn("-test.run=^" + authorization.TOP + "$", args)
        self.assertNotIn("-test.run=^" + sender.TOP + "$", args)
        self.assertIn("PHOENIX_M8_SSH_AUTHORIZATION_SANDBOX=1", args)
        self.assertEqual(args[:3], ["create", "--pull", "never"])
        self.assertEqual(args.count("--cap-add"), 4)
        self.assertNotIn("--privileged", args)
        self.assertNotIn("--mount", args)
        self.assertNotIn("--volume", args)

    def test_default_and_carrier_are_unchanged(self):
        for runtime in (False, True):
            default = sandbox.create_arguments("owned", "cached", runtime, "arm64")
            self.assertEqual(default, sandbox.create_arguments("owned", "cached", runtime, "arm64", False))
            self.assertNotIn("PHOENIX_M8_SSH_AUTHORIZATION_SANDBOX=1", default)
        self.assertEqual(sandbox.create_arguments("owned", "cached", False, "arm64", True),
                         sandbox.create_arguments("owned", "cached", False, "arm64", False))


if __name__ == "__main__":
    unittest.main()
