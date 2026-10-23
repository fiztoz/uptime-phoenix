"""Offline tests for the M8 AWS acceptance harness. The aws CLI is always an injected fake."""
import base64
import contextlib
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import m8_aws_acceptance as h

NOW = dt.datetime(2026, 10, 24, 3, 0, 0, tzinfo=dt.timezone.utc)
ACCOUNT = "123456789012"
CIDR = "203.0.113.7/32"
REGION = "ap-southeast-1"
KEY_BODY = "TOPSECRETKEYBODYb3BlbnNzaC1rZXktdjE"
PEM_TEXT = f"-----BEGIN OPENSSH PRIVATE KEY-----\n{KEY_BODY}\n-----END OPENSSH PRIVATE KEY-----\n"
AMI = {"arm64": "ami-0a0a0a0a", "amd64": "ami-0b0b0b0b"}
OTHER_RUN = "aaaaaaaaaaaa"


def host_key(seed, key_type="ssh-ed25519"):
    """(type, base64, SHA256 fingerprint) of a synthetic public key, computed independently of the harness."""
    blob = struct.pack(">I", len(key_type)) + key_type.encode() + struct.pack(">I", 32) + bytes([seed]) * 32
    return key_type, base64.b64encode(blob).decode(), "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")


def genuine_keys(seed):
    return [host_key(seed), host_key(seed + 100, "ssh-rsa")]


ATTACKER = host_key(250)


def console_text(keys, fingerprints=True, key_block=True, extra=(), prefix=""):
    lines = ["[    3.1] cloud-init boot", f"{prefix}ssh-ed25519 {ATTACKER[1]} forged@outside-any-block"]
    if fingerprints:
        lines += ["-----BEGIN SSH HOST KEY FINGERPRINTS-----"]
        lines += [f"256 {fp} root@ip-10-0-0-1 ({'ED25519' if t == 'ssh-ed25519' else 'RSA'})" for t, _, fp in keys]
        lines += ["-----END SSH HOST KEY FINGERPRINTS-----"]
    if key_block:
        lines += ["-----BEGIN SSH HOST KEY KEYS-----"]
        lines += [f"{t} {b64} root@ip-10-0-0-1" for t, b64, _ in [*keys, *extra]]
        lines += ["-----END SSH HOST KEY KEYS-----"]
    return "\n".join(prefix + line if prefix and not line.startswith(prefix) else line for line in lines) + "\n"


def opts(args, start=2):
    out, current = {}, None
    for arg in args[start:]:
        if arg.startswith("--"):
            current = out.setdefault(arg, [])
        elif current is not None:
            current.append(arg)
    return out


def tags_from(spec_json, resource_type):
    return next(s["Tags"] for s in json.loads(spec_json) if s["ResourceType"] == resource_type)


class Fail(Exception):
    def __init__(self, rc, stderr):
        super().__init__(stderr)
        self.rc, self.stderr = rc, stderr


class FakeAws:
    """A stateful fake `aws` runner: records every call and emulates just enough EC2/STS/SSM."""

    def __init__(self, account=ACCOUNT, ignore_filters=False, profile=None):
        self.account, self.ignore_filters, self.profile = account, ignore_filters, profile
        self.calls, self.instances, self.sgs, self.keys, self.deleted = [], {}, {}, {}, []
        self.failures, self.n = {}, 0
        self.console_mode, self.attacker_in_keys = "full", False
        self.http_tokens, self.owner, self.vpcs, self.tamper_sg, self.tamper_instances = "required", h.CANONICAL_OWNER, 1, False, False

    def __call__(self, argv, timeout):
        prefix = ["aws", "--region", REGION, "--output", "json", "--no-cli-pager"] + (["--profile", self.profile] if self.profile else [])
        assert argv[:len(prefix)] == prefix, argv
        args = argv[len(prefix):]
        assert "--profile" not in args, argv
        self.calls.append(args)
        queue = self.failures.get((args[0], args[1]))
        if queue:
            rc, stderr = queue.pop(0)
            return rc, "", stderr
        try:
            result = getattr(self, "op_" + args[1].replace("-", "_"))(args)
        except Fail as failure:
            return failure.rc, "", failure.stderr
        return 0, json.dumps(result) if result else "", ""

    def next_id(self, prefix):
        self.n += 1
        return f"{prefix}-{self.n:08x}"

    def select(self, store, args, id_flag):
        o = opts(args)
        if id_flag in o:  # by-id describes are exact even when filters are unreliable
            return [store[i] for i in o[id_flag]]
        if self.ignore_filters or "--filters" not in o:
            return list(store.values())
        return [item for item in store.values() if all(self.passes(item, f) for f in json.loads(o["--filters"][0]))]

    @staticmethod
    def passes(item, flt):
        name, values = flt["Name"], flt["Values"]
        if name.startswith("tag:"):
            return {t["Key"]: t["Value"] for t in item.get("Tags", [])}.get(name[4:]) in values
        return item.get({"key-name": "KeyName", "group-name": "GroupName"}[name]) in values

    # seeded foreign/other-run world -------------------------------------------------
    def add_instance(self, iid, tags, state="running"):
        item = {"InstanceId": iid, "State": {"Name": state}, "InstanceType": "t3.micro", "SecurityGroups": []}
        if tags is not None:
            item["Tags"] = [{"Key": k, "Value": v} for k, v in tags.items()]
        self.instances[iid] = item

    def add_sg(self, gid, name, tags):
        self.sgs[gid] = {"GroupId": gid, "GroupName": name, "Tags": [{"Key": k, "Value": v} for k, v in tags.items()]}

    def add_key(self, kid, name, tags):
        self.keys[kid] = {"KeyPairId": kid, "KeyName": name, "Tags": [{"Key": k, "Value": v} for k, v in tags.items()]}

    def seed_foreign(self, run_id):
        own = {"Project": h.PROJECT, "RunId": OTHER_RUN, "ExpiresAt": "2026-10-24T05:00:00Z"}
        self.add_instance("i-0f000001", {"Project": "other-project", "RunId": run_id})  # same RunId, wrong Project
        self.add_instance("i-0f000002", own)  # another run of this project
        self.add_instance("i-0f000003", None)  # untagged
        self.add_sg("sg-0f000001", "default", {})
        self.add_sg("sg-0f000002", f"m8-{OTHER_RUN}", own)
        self.add_key("key-0f000001", "laptop", {})
        self.add_key("key-0f000002", f"m8-{OTHER_RUN}", own)

    def snapshot(self, only_foreign=False):
        keep = (lambda k: "-0f" in k) if only_foreign else (lambda k: True)
        return ({k: v["State"]["Name"] for k, v in self.instances.items() if keep(k)},
                {k for k in self.sgs if keep(k)}, {k for k in self.keys if keep(k)})

    # operations ----------------------------------------------------------------------
    def op_get_caller_identity(self, args):
        return {"UserId": "AIDAEXAMPLE", "Account": self.account, "Arn": f"arn:aws:iam::{self.account}:user/t"}

    def op_describe_vpcs(self, args):
        return {"Vpcs": [{"VpcId": f"vpc-0a1b2c3{i}", "IsDefault": True} for i in range(self.vpcs)]}

    def op_describe_subnets(self, args):
        return {"Subnets": [{"SubnetId": f"subnet-0000000{z}", "VpcId": "vpc-0a1b2c30", "AvailabilityZone": REGION + z,
                             "State": "available", "MapPublicIpOnLaunch": True, "DefaultForAz": True} for z in "abc"]}

    def op_describe_instance_type_offerings(self, args):  # zone a lacks t4g.small
        rows = [("a", "t3.small")] + [(z, t) for z in "bc" for t in ("t3.small", "t4g.small")]
        return {"InstanceTypeOfferings": [{"InstanceType": t, "LocationType": "availability-zone", "Location": REGION + z} for z, t in rows]}

    def op_get_parameter(self, args):
        name = opts(args)["--name"][0]
        return {"Parameter": {"Name": name, "Value": AMI["arm64" if "/arm64/" in name else "amd64"]}}

    def op_describe_images(self, args):
        ami = opts(args)["--image-ids"][0]
        arch = "arm64" if ami == AMI["arm64"] else "x86_64"
        return {"Images": [{"ImageId": ami, "OwnerId": self.owner, "Architecture": arch, "State": "available", "RootDeviceName": "/dev/sda1"}]}

    def op_create_security_group(self, args):
        o, gid = opts(args), self.next_id("sg")
        self.sgs[gid] = {"GroupId": gid, "GroupName": o["--group-name"][0], "Ingress": [],
                         "Tags": tags_from(o["--tag-specifications"][0], "security-group")}
        return {"GroupId": gid}

    def op_authorize_security_group_ingress(self, args):
        o = opts(args)
        self.sgs[o["--group-id"][0]]["Ingress"] += json.loads(o["--ip-permissions"][0])
        return {"Return": True}

    def op_create_key_pair(self, args):
        o, kid = opts(args), self.next_id("key")
        self.keys[kid] = {"KeyPairId": kid, "KeyName": o["--key-name"][0], "Tags": tags_from(o["--tag-specifications"][0], "key-pair")}
        return {"KeyName": o["--key-name"][0], "KeyPairId": kid, "KeyFingerprint": "aa:bb", "KeyMaterial": PEM_TEXT}

    def op_run_instances(self, args):
        o, iid = opts(args), self.next_id("i")
        self.instances[iid] = {
            "InstanceId": iid, "State": {"Name": "pending"}, "InstanceType": o["--instance-type"][0], "KeyName": o["--key-name"][0],
            "ImageId": o["--image-id"][0], "SecurityGroups": [{"GroupId": o["--security-group-ids"][0]}],
            "PublicIpAddress": f"198.51.100.{self.n}", "PrivateIpAddress": f"172.31.0.{self.n}",
            "MetadataOptions": {"HttpTokens": self.http_tokens}, "Tags": tags_from(o["--tag-specifications"][0], "instance"), "_launch": o}
        return {"Instances": [{"InstanceId": iid, "InstanceType": o["--instance-type"][0]}]}

    def op_wait(self, args):
        for iid in opts(args, 3)["--instance-ids"]:
            self.instances[iid]["State"]["Name"] = {"instance-running": "running", "instance-terminated": "terminated"}.get(
                args[2], self.instances[iid]["State"]["Name"])
        return {}

    def op_describe_instances(self, args):
        if self.tamper_instances and "--instance-ids" in opts(args):  # ownership changes between discovery and terminate
            for iid in opts(args)["--instance-ids"]:
                self.instances[iid]["Tags"] = [{"Key": "Project", "Value": "someone-else"}]
        items = [{k: v for k, v in i.items() if not k.startswith("_")} for i in self.select(self.instances, args, "--instance-ids")]
        return {"Reservations": [{"Instances": [i]} for i in items]}

    def op_describe_instance_attribute(self, args):
        iid = opts(args)["--instance-id"][0]
        return {"InstanceId": iid, "InstanceInitiatedShutdownBehavior": {
            "Value": self.instances[iid]["_launch"].get("--instance-initiated-shutdown-behavior", ["stop"])[0]}}

    def op_get_console_output(self, args):
        iid = opts(args)["--instance-id"][0]
        if self.console_mode == "none":
            return {"InstanceId": iid}
        keys = genuine_keys(1 if self.instances[iid]["InstanceType"] == "t4g.small" else 2)
        return {"InstanceId": iid, "Output": console_text(keys, key_block=self.console_mode == "full",
                                                          extra=[ATTACKER] if self.attacker_in_keys else [])}

    def op_terminate_instances(self, args):
        iid = opts(args)["--instance-ids"][0]
        self.instances[iid]["State"]["Name"] = "shutting-down"
        self.deleted.append(("terminate", iid))
        return {"TerminatingInstances": [{"InstanceId": iid, "CurrentState": {"Name": "shutting-down"}}]}

    def op_describe_key_pairs(self, args):
        return {"KeyPairs": self.select(self.keys, args, "--key-pair-ids")}

    def op_delete_key_pair(self, args):
        kid = opts(args)["--key-pair-id"][0]
        del self.keys[kid]
        self.deleted.append(("delete-key", kid))
        return {"Return": True, "KeyPairId": kid}

    def op_describe_security_groups(self, args):
        if self.tamper_sg and "--group-ids" in opts(args):  # ownership changes between discovery and delete
            for gid in opts(args)["--group-ids"]:
                self.sgs[gid]["Tags"] = [{"Key": "Project", "Value": "someone-else"}]
        return {"SecurityGroups": self.select(self.sgs, args, "--group-ids")}

    def op_delete_security_group(self, args):
        gid = opts(args)["--group-id"][0]
        if any(i["State"]["Name"] != "terminated" and gid in [g["GroupId"] for g in i["SecurityGroups"]] for i in self.instances.values()):
            raise Fail(254, "An error occurred (DependencyViolation) when calling DeleteSecurityGroup")
        del self.sgs[gid]
        self.deleted.append(("delete-sg", gid))
        return {"Return": True, "GroupId": gid}


class HarnessCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name) / "tmp"
        self.fake = FakeAws()
        self.sleeps, self.ssh_calls, self.keyscan_calls, self.keyscan_map = [], [], [], {}

    def keyscan(self, host):
        self.keyscan_calls.append(host)
        return self.keyscan_map.get(host, "")

    def ssh(self, argv, env):
        self.ssh_calls.append((argv, env))
        return 7

    def main(self, argv, fake=None):
        argv = list(argv)
        extra = ["--evidence-root", str(self.root)]
        if argv and argv[0] in h.SUBCOMMANDS:
            argv[1:1] = extra
        else:
            argv = extra + argv
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = h.main(argv, runner=fake or self.fake, sleep=self.sleeps.append, now=lambda: NOW,
                          keyscan=self.keyscan, ssh_runner=self.ssh)
        return code, out.getvalue(), err.getvalue()

    def plan(self, minutes=120):
        code, out, _ = self.main(["plan", "--operator-cidr", CIDR, "--max-minutes", str(minutes)])
        self.assertEqual(code, 0)
        return re.search(r"run id\s+: ([0-9a-f]{12})", out).group(1)

    def up_args(self, run_id, **over):
        args = {"--approve-run-id": run_id, "--operator-cidr": CIDR, "--expect-account": ACCOUNT, "--max-minutes": "120"}
        args.update(over)
        return ["up", *[x for pair in args.items() for x in pair]]

    def up(self, minutes=120):
        run_id = self.plan(minutes)
        code, out, err = self.main(self.up_args(run_id, **{"--max-minutes": str(minutes)}))
        self.assertEqual(code, 0, err)
        return run_id, json.loads(out)

    def edir(self, run_id):
        return self.root / f"m8-aws-{run_id}"

    def ops(self, calls=None):
        return [(c[1] if c[1] != "wait" else "wait:" + c[2]) for c in (self.fake.calls if calls is None else calls)]

    def calls_of(self, op):
        return [c for c in self.fake.calls if c[1] == op]

    def mutations(self, calls=None):
        return [c for c in (self.fake.calls if calls is None else calls) if (c[0], c[1]) in h.MUTATING_OPS]


class PlanTests(HarnessCase):
    def test_plan_makes_zero_aws_and_zero_subprocess_calls(self):
        with mock.patch.object(h.subprocess, "run", side_effect=AssertionError("subprocess")), \
                mock.patch.object(h.subprocess, "Popen", side_effect=AssertionError("subprocess")):
            code, out, _ = self.main(["plan", "--operator-cidr", CIDR])
        self.assertEqual(code, 0)
        self.assertEqual(self.fake.calls, [])
        self.assertIn("no aws command was run", out)

    def test_plan_prints_resources_commands_cost_and_teardown(self):
        run_id = self.plan()
        out = self.main(["plan", "--operator-cidr", CIDR])[1]
        for text in ("t4g.small", "t3.small", CIDR, "authorize-security-group-ingress", "create-key-pair", "--key-type ed25519",
                     "instance-initiated-shutdown-behavior terminate", "shutdown -h +120", "Teardown plan", "delete-security-group",
                     "/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id",
                     f"~${h.estimate(120)['hourly_usd']}/hour", "To approve, run exactly"):
            self.assertIn(text, out)
        self.assertNotIn("0.0.0.0/0", out)
        self.assertRegex(out, r"run id\s+: [0-9a-f]{12}")
        self.assertNotIn(run_id, out)  # every plan mints a fresh run id

    def test_default_subcommand_is_plan(self):
        code, out, _ = self.main(["--operator-cidr", CIDR])
        self.assertEqual((code, self.fake.calls), (0, []))
        self.assertIn("PLAN", out)

    def test_plan_persists_private_plan_only(self):
        run_id = self.plan()
        edir = self.edir(run_id)
        self.assertEqual(stat.S_IMODE(edir.stat().st_mode), 0o700)
        self.assertEqual(sorted(p.name for p in edir.iterdir()), ["plan.json"])
        self.assertEqual(stat.S_IMODE((edir / "plan.json").stat().st_mode), 0o600)

    def test_plan_rejects_bad_cidr_and_minutes_without_side_effects(self):
        bad = ["0.0.0.0/0", "0.0.0.0/32", "10.0.0.0/8", "203.0.113.7/24", "203.0.113.7", "203.0.113.256/32", "::1/128", " 203.0.113.7/32",
               "203.0.113.7/32\n", "127.0.0.1/32", "224.0.0.1/32", "169.254.1.1/32", "255.255.255.255/32", "203.000.113.7/32", ""]
        for cidr in bad:
            with self.subTest(cidr=cidr):
                code, _, err = self.main(["plan", "--operator-cidr", cidr])
                self.assertEqual(code, 1)
                self.assertIn("refused: operator_cidr", err)
        for minutes in ("5", "481", "0", "-1"):
            with self.subTest(minutes=minutes):
                self.assertEqual(self.main(["plan", "--operator-cidr", CIDR, "--max-minutes", minutes])[0], 1)
        self.assertFalse(self.root.exists() and any(self.root.iterdir()))
        self.assertEqual(self.fake.calls, [])

    def test_missing_cidr_is_refused(self):
        with self.assertRaises(SystemExit) as raised:
            self.main(["plan"])
        self.assertNotEqual(raised.exception.code, 0)

    def test_double_dash_only_for_ssh(self):
        self.assertEqual(self.main(["plan", "--operator-cidr", CIDR, "--", "id"])[0], 1)

    def test_validators(self):
        self.assertEqual(h.validate_cidr(CIDR), CIDR)
        for bad in ("12345678901", "1234567890123", "12345678901a", "", None):
            with self.assertRaises(h.HarnessError):
                h.validate_account(bad)
        for bad in ("../../x", "ABCDEF123456", "abc", "0123456789abcdef", "0123456789ab\n"):
            with self.assertRaises(h.HarnessError):
                h.validate_run_id(bad)


class ProfileTests(HarnessCase):
    def test_profile_defaults_to_none_and_is_validated(self):
        out = self.main(["plan", "--operator-cidr", CIDR])[1]
        self.assertNotIn("--profile", out)
        for bad in ("bad profile", "--x", "a;b", "", "-lead", "a" * 65, "x\n"):
            with self.subTest(profile=bad):
                self.assertEqual(self.main(["plan", "--operator-cidr", CIDR, f"--profile={bad}"])[0], 1)
        self.assertEqual(self.fake.calls, [])

    def test_plan_shows_the_profile_on_every_command_and_in_follow_ups(self):
        code, out, _ = self.main(["plan", "--operator-cidr", CIDR, "--profile", "phoenix"])
        self.assertEqual((code, self.fake.calls), (0, []))
        commands = [line.strip() for line in out.splitlines() if line.strip().startswith("aws --region")]
        self.assertGreater(len(commands), 20)
        self.assertTrue(all(" --no-cli-pager --profile phoenix " in c for c in commands), commands)
        self.assertRegex(out, r"aws profile\s+: phoenix")
        approve = out.splitlines()[-1]
        self.assertTrue(approve.strip().startswith("python3 scripts/m8_aws_acceptance.py up --approve-run-id"))
        self.assertTrue(approve.endswith("--profile phoenix"))

    def test_every_aws_call_of_up_down_and_sweep_carries_the_profile(self):
        seen = []
        fake = FakeAws(profile="phoenix")

        def runner(argv, timeout):
            seen.append(argv)
            return fake(argv, timeout)

        run_id = self.plan()
        code, out, err = self.main(self.up_args(run_id) + ["--profile", "phoenix"], fake=runner)
        self.assertEqual(code, 0, err)
        self.assertIn("--profile phoenix", json.loads(out)["teardown"])
        self.assertEqual(self.main(["down", "--run-id", run_id, "--profile", "phoenix"], fake=runner)[0], 0)
        self.assertEqual(self.main(["sweep", "--profile", "phoenix"], fake=runner)[0], 0)
        self.assertGreater(len(seen), 30)
        for argv in seen:
            self.assertEqual(argv[6:8], ["--profile", "phoenix"], argv)
            self.assertEqual(argv.count("--profile"), 1)

    def test_a_caller_cannot_inject_its_own_profile_or_region(self):
        client = h.AwsCli(REGION, lambda argv, timeout: (0, "{}", ""), profile="phoenix")
        for flag in ("--profile", "--region", "--output"):
            with self.assertRaises(h.HarnessError):
                client.run_aws(["ec2", "describe-vpcs", flag, "x"])
        with self.assertRaises(h.HarnessError):
            h.AwsCli(REGION, profile="bad profile")


class UpRefusalTests(HarnessCase):
    def assertRefused(self, argv, reason, calls=0):
        start = len(self.fake.calls)
        code, _, err = self.main(argv)
        self.assertEqual(code, 1, err)
        self.assertIn(reason, err)
        self.assertEqual(len(self.fake.calls) - start, calls)
        self.assertEqual(self.mutations(self.fake.calls[start:]), [])

    def test_missing_approval_or_account_is_refused_by_the_parser(self):
        run_id = self.plan()
        for drop in ("--approve-run-id", "--expect-account", "--operator-cidr"):
            with self.subTest(drop=drop), self.assertRaises(SystemExit):
                args = self.up_args(run_id)
                i = args.index(drop)
                del args[i:i + 2]
                self.main(args)
        self.assertEqual(self.fake.calls, [])

    def test_run_id_not_printed_by_plan_is_refused(self):
        self.plan()
        self.assertRefused(self.up_args("0123456789ab"), "run_id_not_printed_by_plan")

    def test_malformed_approval_is_refused(self):
        for value in ("../../etc", "short", "0123456789AB", "0123456789abcdef", "0123456789ab/../x"):
            with self.subTest(value=value):
                self.assertRefused(self.up_args(value), "invalid_run_id")
        self.assertFalse(any(p.name == "etc" for p in self.root.parent.rglob("etc")))

    def test_parameters_must_match_the_plan(self):
        run_id = self.plan()
        self.assertRefused(self.up_args(run_id, **{"--operator-cidr": "203.0.113.8/32"}), "plan_mismatch")
        self.assertRefused(self.up_args(run_id, **{"--max-minutes": "60"}), "plan_mismatch")
        self.assertRefused(self.up_args(run_id) + ["--region", "us-east-1"], "plan_mismatch")

    def test_bad_cidr_at_up_is_refused_before_any_call(self):
        run_id = self.plan()
        for cidr in ("0.0.0.0/0", "10.0.0.0/8", "203.0.113.7/31", "203.0.113.7"):
            with self.subTest(cidr=cidr):
                self.assertRefused(self.up_args(run_id, **{"--operator-cidr": cidr}), "operator_cidr")

    def test_malformed_account_is_refused_before_any_call(self):
        run_id = self.plan()
        for account in ("1234", "12345678901a", "1234567890123"):
            with self.subTest(account=account):
                self.assertRefused(self.up_args(run_id, **{"--expect-account": account}), "invalid_account_id")

    def test_account_mismatch_makes_only_the_identity_call(self):
        run_id = self.plan()
        self.assertRefused(self.up_args(run_id, **{"--expect-account": "999999999999"}), "account_mismatch", calls=1)
        self.assertEqual(self.fake.calls[0][:2], ["sts", "get-caller-identity"])

    def test_run_id_is_single_use(self):
        run_id, _ = self.up()
        self.assertRefused(self.up_args(run_id), "run_id_already_used")

    def test_discovery_failures_create_nothing(self):
        cases = {
            "default_vpc_not_unique": lambda f: setattr(f, "vpcs", 2),
            "ami_not_official_canonical": lambda f: setattr(f, "owner", "111122223333"),
        }
        for reason, tweak in cases.items():
            with self.subTest(reason=reason):
                self.setUp()
                tweak(self.fake)
                run_id = self.plan()
                code, _, err = self.main(self.up_args(run_id))
                self.assertEqual(code, 1)
                self.assertIn(reason, err)
                self.assertEqual(self.mutations(), [])

    def test_unexpected_json_shapes_fail_closed(self):
        for body, reason in (("[]", "aws_output_not_object"), ("not json", "aws_output_not_json"), ('{"Arn": "x"}', "unexpected_aws_response:Account"),
                             ('{"Account": "12"}', "unexpected_aws_response:Account")):
            with self.subTest(body=body):
                run_id = self.plan()
                code, _, err = self.main(self.up_args(run_id), fake=lambda argv, timeout: (0, body, ""))
                self.assertEqual(code, 1)
                self.assertIn(reason, err)

    def test_failed_aws_call_is_refused_with_redacted_stderr(self):
        run_id = self.plan()
        leak = f"denied {PEM_TEXT}"
        code, _, err = self.main(self.up_args(run_id), fake=lambda argv, timeout: (255, "", leak))
        self.assertEqual(code, 1)
        self.assertIn("aws_failed:sts get-caller-identity", err)
        self.assertNotIn(KEY_BODY, err)


class UpTests(HarnessCase):
    def test_exact_create_calls_and_tags(self):
        run_id, summary = self.up()
        expires = "2026-10-24T05:00:00Z"
        want = lambda name: [{"Key": "Project", "Value": "uptime-phoenix-m8"}, {"Key": "RunId", "Value": run_id},
                             {"Key": "ExpiresAt", "Value": expires}, {"Key": "Name", "Value": name}]
        self.assertEqual(self.ops(self.mutations()), ["create-security-group", "authorize-security-group-ingress", "create-key-pair",
                                                      "run-instances", "run-instances"])
        sg = opts(self.calls_of("create-security-group")[0])
        self.assertEqual(sg["--group-name"], [f"m8-{run_id}"])
        self.assertEqual(json.loads(sg["--tag-specifications"][0]), [{"ResourceType": "security-group", "Tags": want(f"m8-{run_id}")}])
        ingress = self.calls_of("authorize-security-group-ingress")
        self.assertEqual(len(ingress), 1)
        self.assertEqual(json.loads(opts(ingress[0])["--ip-permissions"][0]), [
            {"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22, "IpRanges": [{"CidrIp": CIDR, "Description": "m8 operator ssh"}]}])
        key = opts(self.calls_of("create-key-pair")[0])
        self.assertEqual((key["--key-name"], key["--key-type"]), ([f"m8-{run_id}"], ["ed25519"]))
        self.assertEqual(json.loads(key["--tag-specifications"][0]), [{"ResourceType": "key-pair", "Tags": want(f"m8-{run_id}")}])
        launches = {opts(c)["--instance-type"][0]: opts(c) for c in self.calls_of("run-instances")}
        self.assertEqual(set(launches), {"t4g.small", "t3.small"})
        for (itype, arch) in (("t4g.small", "arm64"), ("t3.small", "amd64")):
            o = launches[itype]
            self.assertEqual(o["--image-id"], [AMI[arch]])
            self.assertEqual(o["--subnet-id"], ["subnet-0000000b"])  # zone a does not offer t4g.small
            self.assertEqual(o["--count"], ["1"])
            self.assertEqual(o["--instance-initiated-shutdown-behavior"], ["terminate"])
            self.assertEqual(o["--client-token"], [f"m8-{run_id}-{arch}"])
            self.assertEqual(json.loads(o["--metadata-options"][0])["HttpTokens"], "required")
            self.assertEqual(json.loads(o["--block-device-mappings"][0]), [{"DeviceName": "/dev/sda1", "Ebs": {
                "VolumeSize": 16, "VolumeType": "gp3", "Encrypted": True, "DeleteOnTermination": True}}])
            self.assertEqual(json.loads(o["--credit-specification"][0]), {"CpuCredits": "standard"})
            specs = json.loads(o["--tag-specifications"][0])
            self.assertEqual({s["ResourceType"] for s in specs}, {"instance", "volume", "network-interface"})
            self.assertTrue(all(s["Tags"] == want(f"m8-{run_id}-{arch}") for s in specs))
            self.assertNotIn("--iam-instance-profile", o)
        script = Path(launches["t4g.small"]["--user-data"][0][len("file://"):]).read_text()
        self.assertIn("shutdown -h +120", script)
        self.assertEqual(len(self.fake.instances), 2)
        for store in (self.fake.instances, self.fake.sgs, self.fake.keys):
            for item in store.values():
                self.assertEqual({t["Key"]: t["Value"] for t in item["Tags"]}["RunId"], run_id)
                self.assertEqual({t["Key"]: t["Value"] for t in item["Tags"]}["ExpiresAt"], expires)
        self.assertNotIn("0.0.0.0/0", json.dumps(self.fake.calls))
        self.assertEqual(summary["expires_at"], expires)

    def test_max_minutes_flows_into_dead_man_switch_and_expiry(self):
        run_id, summary = self.up(minutes=45)
        script = (self.edir(run_id) / "user-data.sh").read_text()
        self.assertIn("shutdown -h +45", script)
        self.assertEqual(summary["expires_at"], "2026-10-24T03:45:00Z")

    def test_call_order_matches_the_printed_plan(self):
        run_id, _ = self.up()
        planned = [(a[0], a[1]) for _, a in h.plan_steps(run_id, CIDR, 120)]
        self.assertEqual([(c[0], c[1]) for c in self.fake.calls], planned)

    def test_key_file_is_private_and_never_logged(self):
        run_id, _ = self.up()
        edir = self.edir(run_id)
        key = edir / "private" / f"m8-{run_id}.pem"
        self.assertEqual(key.read_text(), PEM_TEXT)
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        for directory in (edir, edir / "private"):
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
        for name in ("state.json", "aws-calls.jsonl", "private/known_hosts", "user-data.sh"):
            self.assertEqual(stat.S_IMODE((edir / name).stat().st_mode), 0o600, name)
            if name != "private/known_hosts":
                self.assertNotIn(KEY_BODY, (edir / name).read_text())
        log = (edir / "aws-calls.jsonl").read_text()
        self.assertIn("[REDACTED]", log)
        self.assertNotIn("PRIVATE KEY", log)

    def test_existing_key_file_is_never_overwritten(self):
        run_id = self.plan()
        private = self.edir(run_id) / "private"
        private.mkdir(mode=0o700)
        (private / f"m8-{run_id}.pem").write_text("pre-existing")
        code, _, err = self.main(self.up_args(run_id))
        self.assertEqual(code, 1)
        self.assertIn("key_file_already_exists", err)
        self.assertEqual((private / f"m8-{run_id}.pem").read_text(), "pre-existing")
        self.assertEqual(self.mutations(), [])  # refused before the key pair was created, so the key is never lost

    def test_known_hosts_contains_only_console_verified_keys(self):
        self.fake.attacker_in_keys = True
        run_id, summary = self.up()
        known = (self.edir(run_id) / "private" / "known_hosts").read_text().splitlines()
        expected = set()
        for arch, seed in (("arm64", 1), ("amd64", 2)):
            ip = summary["instances"][arch]["public_ip"]
            expected |= {f"{ip} {t} {b64}" for t, b64, _ in genuine_keys(seed)}
            self.assertEqual(summary["instances"][arch]["host_key_fingerprints"], sorted(fp for _, _, fp in genuine_keys(seed)))
        self.assertEqual(set(known), expected)
        self.assertEqual(len(known), len(expected))
        self.assertNotIn(ATTACKER[1], "\n".join(known))
        self.assertEqual(self.keyscan_calls, [])  # console keys were sufficient

    def test_keyscan_fallback_is_accepted_only_when_fingerprints_match(self):
        self.fake.console_mode = "fp_only"
        run_id = self.plan()

        def scan(host):  # the genuine ed25519 key plus an attacker key, for whichever instance owns this IP
            inst = next(i for i in self.fake.instances.values() if i["PublicIpAddress"] == host)
            good = genuine_keys(1 if inst["InstanceType"] == "t4g.small" else 2)[0]
            return f"{host} {good[0]} {good[1]}\n{host} ssh-ed25519 {ATTACKER[1]}\n"

        self.keyscan = scan
        code, out, err = self.main(self.up_args(run_id))
        self.assertEqual(code, 0, err)
        known = (self.edir(run_id) / "private" / "known_hosts").read_text().splitlines()
        self.assertEqual(len(known), 2)
        self.assertNotIn(ATTACKER[1], "\n".join(known))

    def test_unverifiable_keyscan_is_refused_and_torn_down(self):
        self.fake.console_mode = "fp_only"
        self.keyscan_map = {f"198.51.100.{n}": f"x ssh-ed25519 {ATTACKER[1]}\n" for n in range(1, 9)}
        run_id = self.plan()
        code, _, err = self.main(self.up_args(run_id))
        self.assertEqual(code, 1)
        self.assertIn("host_key_unverifiable", err)
        self.assertFalse((self.edir(run_id) / "private" / "known_hosts").exists())

    def test_missing_fingerprints_refuse_and_tear_down_only_this_run(self):
        run_id = self.plan()
        self.fake.seed_foreign(run_id)
        before = self.fake.snapshot(only_foreign=True)
        self.fake.console_mode = "none"
        code, _, err = self.main(self.up_args(run_id))
        self.assertEqual(code, 1)
        self.assertIn("host_key_fingerprints_unavailable", err)
        self.assertLessEqual(len(self.sleeps), 40)
        self.assertTrue(self.sleeps)
        self.assertFalse((self.edir(run_id) / "private" / "known_hosts").exists())
        self.assertEqual(self.ssh_calls, [])
        self.assertEqual(self.fake.snapshot(only_foreign=True), before)  # foreign resources untouched
        self.assertEqual([i["State"]["Name"] for k, i in self.fake.instances.items() if "-0f" not in k], ["terminated"] * 2)
        self.assertEqual(len([k for k in self.fake.sgs if "-0f" not in k] + [k for k in self.fake.keys if "-0f" not in k]), 0)
        self.assertEqual(len(self.fake.deleted), 4)
        state = json.loads((self.edir(run_id) / "state.json").read_text())
        self.assertEqual(state["status"], "failed")

    def test_keep_on_failure_leaves_resources_for_an_explicit_down(self):
        self.fake.console_mode = "none"
        run_id = self.plan()
        code, _, _ = self.main(self.up_args(run_id) + ["--keep-on-failure"])
        self.assertEqual(code, 1)
        self.assertEqual(self.fake.deleted, [])
        self.assertEqual((len(self.fake.instances), len(self.fake.sgs), len(self.fake.keys)), (2, 1, 1))

    def test_imds_and_shutdown_behavior_are_verified(self):
        self.fake.http_tokens = "optional"
        run_id = self.plan()
        code, _, err = self.main(self.up_args(run_id))
        self.assertEqual((code, "instance_verification_failed" in err), (1, True))

    def test_failure_before_any_create_does_not_teardown(self):
        self.fake.owner = "111122223333"
        run_id = self.plan()
        self.main(self.up_args(run_id))
        self.assertEqual([c for c in self.fake.calls if c[1].startswith(("delete", "terminate"))], [])


class DownTests(HarnessCase):
    def test_down_deletes_only_resources_tagged_with_the_run_id(self):
        for ignore_filters in (False, True):
            with self.subTest(ignore_filters=ignore_filters):
                self.setUp()
                self.fake = FakeAws(ignore_filters=ignore_filters)
                run_id = self.plan()
                self.fake.seed_foreign(run_id)
                foreign_before = self.fake.snapshot(only_foreign=True)
                code, out, err = self.main(self.up_args(run_id))
                self.assertEqual(code, 0, err)
                ours = {x["instance_id"] for x in json.loads(out)["instances"].values()}
                code, out, err = self.main(["down", "--run-id", run_id])
                self.assertEqual(code, 0, err)
                self.assertEqual(self.fake.snapshot(only_foreign=True), foreign_before)
                self.assertEqual({i for kind, i in self.fake.deleted if kind == "terminate"}, ours)
                self.assertEqual(len([d for d in self.fake.deleted if d[0] == "delete-key"]), 1)
                self.assertEqual(len([d for d in self.fake.deleted if d[0] == "delete-sg"]), 1)
                self.assertTrue(all("-0f" not in i for _, i in self.fake.deleted))
                report = json.loads(out)
                self.assertEqual(set(report["terminated"]), ours)
                self.assertGreaterEqual(report["foreign_skipped"], 1 if ignore_filters else 0)

    def test_teardown_order_waits_for_termination_and_reverifies_before_each_delete(self):
        run_id, _ = self.up()
        start = len(self.fake.calls)
        self.assertEqual(self.main(["down", "--run-id", run_id])[0], 0)
        calls = self.fake.calls[start:]
        ops = self.ops(calls)
        self.assertEqual(ops[0], "get-caller-identity")
        first = lambda op: ops.index(op)
        self.assertLess(first("terminate-instances"), first("wait:instance-terminated"))
        self.assertLess(first("wait:instance-terminated"), first("delete-key-pair"))
        self.assertLess(first("delete-key-pair"), first("delete-security-group"))
        for i, call in enumerate(calls):
            if call[1] in ("delete-key-pair", "delete-security-group"):
                prev = calls[i - 1]
                self.assertTrue(prev[1].startswith("describe-"), prev)
                self.assertEqual(call[-1], prev[-1])  # the delete targets exactly the id just re-verified
            if call[1] == "terminate-instances":
                self.assertEqual(calls[i - 1][1:3], ["describe-instances", "--instance-ids"])
                self.assertEqual(calls[i - 1][-1], call[-1])
        planned = []
        for _, a in h.teardown_steps(run_id):
            planned.append(a[1] if a[1] != "wait" else "wait:" + a[2])
        order = lambda seq: sorted(set(seq), key=seq.index)
        self.assertEqual(order(ops[1:]), order(planned))
        self.assertEqual({i["State"]["Name"] for i in self.fake.instances.values()}, {"terminated"})
        self.assertEqual((self.fake.sgs, self.fake.keys), ({}, {}))
        self.assertFalse((self.edir(run_id) / "private" / f"m8-{run_id}.pem").exists())  # key pair gone: local key removed
        self.assertEqual(json.loads((self.edir(run_id) / "state.json").read_text())["status"], "torn_down")

    def test_down_is_idempotent(self):
        run_id, _ = self.up()
        self.assertEqual(self.main(["down", "--run-id", run_id])[0], 0)
        deleted = list(self.fake.deleted)
        start = len(self.fake.calls)
        code, out, _ = self.main(["down", "--run-id", run_id])
        self.assertEqual(code, 0)
        self.assertEqual(self.fake.deleted, deleted)
        self.assertEqual(self.mutations(self.fake.calls[start:]), [])
        self.assertEqual(json.loads(out)["terminated"], [])

    def test_a_name_collision_without_the_run_tags_is_not_deleted(self):
        run_id = self.plan()
        self.fake.add_sg("sg-0dec0de1", f"m8-{run_id}", {"Project": "other-project"})
        self.fake.add_key("key-0dec0de1", f"m8-{run_id}", {"Project": h.PROJECT, "RunId": OTHER_RUN})
        code, out, _ = self.main(["down", "--run-id", run_id, "--expect-account", ACCOUNT])
        self.assertEqual(code, 0)
        self.assertEqual(self.fake.deleted, [])
        self.assertEqual(json.loads(out)["foreign_skipped"], 2)

    def test_ownership_change_between_discovery_and_delete_refuses(self):
        run_id, _ = self.up()
        self.fake.tamper_sg = True
        code, _, err = self.main(["down", "--run-id", run_id])
        self.assertEqual(code, 1)
        self.assertIn("ownership_unverified", err)
        self.assertEqual([d for d in self.fake.deleted if d[0] == "delete-sg"], [])

    def test_instance_ownership_change_before_terminate_refuses(self):
        run_id, _ = self.up()
        self.fake.tamper_instances = True
        code, _, err = self.main(["down", "--run-id", run_id])
        self.assertEqual(code, 1)
        self.assertIn("ownership_unverified", err)
        self.assertEqual(self.fake.deleted, [])

    def test_security_group_dependency_is_retried_then_deleted(self):
        run_id, _ = self.up()
        self.fake.failures[("ec2", "delete-security-group")] = [(254, "An error occurred (DependencyViolation)")] * 2
        self.assertEqual(self.main(["down", "--run-id", run_id])[0], 0)
        self.assertEqual(self.sleeps.count(10), 2)
        self.assertEqual(len([d for d in self.fake.deleted if d[0] == "delete-sg"]), 1)

    def test_down_refuses_other_account_or_region_and_unknown_account(self):
        run_id, _ = self.up()
        deleted = list(self.fake.deleted)
        other = FakeAws(account="999999999999")
        self.assertEqual(self.main(["down", "--run-id", run_id], fake=other)[0], 1)
        self.assertEqual([c for c in other.calls if c[1] != "get-caller-identity"], [])
        code, _, err = self.main(["down", "--run-id", run_id, "--region", "us-east-1"])
        self.assertIn("region_mismatch", err)
        code, _, err = self.main(["down", "--run-id", run_id, "--expect-account", "999999999999"])
        self.assertIn("account_mismatch", err)
        code, _, err = self.main(["down", "--run-id", "0123456789ab"])
        self.assertIn("account_unknown_pass_expect_account", err)
        self.assertEqual(self.main(["down", "--run-id", "NOT-HEX"])[0], 1)
        self.assertEqual(self.fake.deleted, deleted)

    def test_down_never_touches_a_failed_describe_shape(self):
        run_id, _ = self.up()
        code, _, err = self.main(["down", "--run-id", run_id], fake=lambda argv, timeout: (0, '{"Account": "123456789012"}' if "sts" in argv else '{"Reservations": "x"}', ""))
        self.assertEqual(code, 1)
        self.assertIn("unexpected_aws_response:Reservations", err)


class SweepTests(HarnessCase):
    def seed(self):
        past, future = "2026-10-24T02:00:00Z", "2026-10-24T09:00:00Z"
        mine = lambda run, exp: {"Project": h.PROJECT, "RunId": run, "ExpiresAt": exp}
        self.fake.add_instance("i-0e000001", mine("bbbbbbbbbbbb", past))
        self.fake.add_instance("i-0e000002", mine("cccccccccccc", future))
        self.fake.add_instance("i-0e000003", mine("dddddddddddd", past), state="terminated")
        self.fake.add_instance("i-0e000004", {"Project": "other-project", "ExpiresAt": past})
        self.fake.add_sg("sg-0e000001", "m8-bbbbbbbbbbbb", mine("bbbbbbbbbbbb", past))
        self.fake.add_key("key-0e000001", "m8-eeeeeeeeeeee", mine("eeeeeeeeeeee", "garbage"))

    def test_sweep_lists_expired_and_never_deletes(self):
        self.seed()
        for ignore in (False, True):
            with self.subTest(ignore_filters=ignore):
                self.fake.ignore_filters, self.fake.calls = ignore, []
                snapshot = self.fake.snapshot()
                code, out, _ = self.main(["sweep", "--expect-account", ACCOUNT])
                self.assertEqual(code, 3)
                report = json.loads(out)
                self.assertEqual({r["id"] for r in report["expired"]}, {"i-0e000001", "sg-0e000001", "key-0e000001"})
                self.assertEqual({r["status"] for r in report["expired"]}, {"expired", "unparseable_expires_at"})
                self.assertEqual(report["active"], 1)
                expired = {r["id"]: r for r in report["expired"]}
                self.assertIn("down --run-id bbbbbbbbbbbb", expired["i-0e000001"]["teardown"])
                self.assertEqual(self.mutations(), [])
                self.assertTrue(all(c[1].startswith("describe") or c[1] == "get-caller-identity" for c in self.fake.calls))
                self.assertEqual(self.fake.snapshot(), snapshot)
                self.assertEqual(self.fake.deleted, [])

    def test_sweep_with_nothing_expired_exits_zero(self):
        self.assertEqual(self.main(["sweep"])[0], 0)

    def test_sweep_command_asks_for_a_read_only_client(self):
        asked, original = [], h.Deps.aws

        def spy(deps, region, edir=None, read_only=False):
            asked.append(read_only)
            return original(deps, region, edir, read_only)

        with mock.patch.object(h.Deps, "aws", spy):
            self.assertEqual(self.main(["sweep"])[0], 0)
        self.assertEqual(asked, [True])

    def test_sweep_client_is_structurally_read_only(self):
        called = []
        client = h.AwsCli(REGION, lambda argv, timeout: called.append(argv) or (0, "{}", ""), read_only=True)
        for args in (["ec2", "terminate-instances", "--instance-ids", "i-0e000001"], ["ec2", "delete-key-pair", "--key-pair-id", "key-1"],
                     ["ec2", "delete-security-group", "--group-id", "sg-1"], ["ec2", "run-instances"]):
            with self.assertRaises(h.HarnessError):
                client.run_aws(args)
        self.assertEqual(called, [])
        client.run_aws(["ec2", "describe-instances"])
        self.assertEqual(len(called), 1)


class AwsCliTests(unittest.TestCase):
    def test_argv_shape_and_no_shell(self):
        completed = subprocess.CompletedProcess([], 0, stdout='{"ok": true}', stderr="")
        with mock.patch.object(h.subprocess, "run", return_value=completed) as run:
            result = h.AwsCli(REGION).run_aws(["sts", "get-caller-identity"])
        self.assertEqual(result, {"ok": True})
        argv = run.call_args.args[0]
        self.assertEqual(argv, ["aws", "--region", REGION, "--output", "json", "--no-cli-pager", "sts", "get-caller-identity"])
        self.assertIs(run.call_args.kwargs["shell"], False)
        self.assertEqual(run.call_args.kwargs["stdin"], subprocess.DEVNULL)

    def test_operation_allow_list_and_global_options(self):
        called = []
        client = h.AwsCli(REGION, lambda argv, timeout: called.append(argv) or (0, "{}", ""))
        bad = [["iam", "create-user"], ["ec2", "modify-security-group-rules"], ["ec2", "authorize-security-group-egress"],
               ["ec2", "wait", "instance-stopped"], ["ec2", "describe-instances", "--region", "us-east-1"],
               ["ec2", "describe-instances", "--profile", "x"], ["ec2"], []]
        for args in bad:
            with self.subTest(args=args), self.assertRaises(h.HarnessError):
                client.run_aws(args)
        self.assertEqual(called, [])
        self.assertEqual(client.run_aws(["ec2", "wait", "instance-running", "--instance-ids", "i-1"]), {})

    def test_empty_output_is_an_empty_object_and_failures_raise(self):
        self.assertEqual(h.AwsCli(REGION, lambda a, t: (0, "  \n", "")).run_aws(["ec2", "describe-vpcs"]), {})
        with self.assertRaises(h.AwsError):
            h.AwsCli(REGION, lambda a, t: (254, "", "boom")).run_aws(["ec2", "describe-vpcs"])

    def test_log_entries_are_redacted(self):
        entries = []
        body = json.dumps({"KeyName": "k", "KeyMaterial": PEM_TEXT})
        h.AwsCli(REGION, lambda a, t: (0, body, f"warn {PEM_TEXT}"), log=entries.append).run_aws(["ec2", "create-key-pair"])
        text = json.dumps(entries)
        self.assertNotIn(KEY_BODY, text)
        self.assertIn("[REDACTED", text)

    def test_redact_handles_truncated_pem(self):
        self.assertNotIn(KEY_BODY, h.redact("x -----BEGIN OPENSSH PRIVATE KEY-----\n" + KEY_BODY))


class HostKeyParsingTests(unittest.TestCase):
    def test_fingerprints_come_only_from_the_last_complete_block(self):
        keys = genuine_keys(3)
        text = console_text(keys, prefix="ec2: ")
        self.assertEqual(h.console_fingerprints(text), {fp for _, _, fp in keys})
        old, new = genuine_keys(4), genuine_keys(5)
        self.assertEqual(h.console_fingerprints(console_text(old) + console_text(new)), {fp for _, _, fp in new})
        self.assertEqual(h.console_fingerprints(console_text(keys, fingerprints=False)), set())
        truncated = console_text(keys).replace("-----END SSH HOST KEY FINGERPRINTS-----", "")
        self.assertEqual(h.console_fingerprints(truncated), set())
        md5 = "-----BEGIN SSH HOST KEY FINGERPRINTS-----\n256 MD5:aa:bb root@x (ED25519)\n-----END SSH HOST KEY FINGERPRINTS-----\n"
        self.assertEqual(h.console_fingerprints(md5), set())

    def test_only_well_formed_keys_in_the_keys_block_are_candidates(self):
        keys = genuine_keys(3)
        lines = h.block_lines(console_text(keys, extra=[ATTACKER]), h.KEY_BLOCK)
        found = h.public_keys(lines + ["ssh-ed25519 !!notbase64!! x", "ssh-ed25519 " + base64.b64encode(b"\x00\x00\x00\x03abcdef").decode()])
        self.assertEqual(found, [(t, b) for t, b, _ in [*keys, ATTACKER]])
        pinned = h.verified_keys(found, {fp for _, _, fp in keys})
        self.assertEqual(pinned, [(t, b) for t, b, _ in keys])

    @unittest.skipUnless(shutil.which("ssh-keygen"), "ssh-keygen not installed")
    def test_fingerprint_formula_matches_ssh_keygen(self):
        key_type, b64, expected = host_key(9)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "k.pub"
            path.write_text(f"{key_type} {b64} test\n")
            out = subprocess.run(["ssh-keygen", "-l", "-f", str(path)], capture_output=True, text=True, check=True).stdout
        self.assertIn(expected, out)
        self.assertEqual(h.key_fingerprint(key_type, b64), expected)


class SshTests(HarnessCase):
    def test_ssh_uses_only_pinned_keys_and_never_the_agent(self):
        run_id, summary = self.up()
        calls_before = len(self.fake.calls)
        with mock.patch.dict(os.environ, {"SSH_AUTH_SOCK": "/tmp/agent.sock", "AWS_SECRET_ACCESS_KEY": "s3cret"}):
            code, _, err = self.main(["ssh", "--run-id", run_id, "--arch", "arm64", "--", "sh", "-c", "echo $HOME; uname -m"])
        self.assertEqual((code, err), (7, ""))  # the fake ssh's exit status is passed through
        self.assertEqual(len(self.fake.calls), calls_before)  # ssh makes no aws calls
        argv, env = self.ssh_calls[0]
        private = self.edir(run_id) / "private"
        ip = summary["instances"]["arm64"]["public_ip"]
        self.assertEqual(argv[0], "ssh")
        for needed in ("BatchMode=yes", "StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "ForwardAgent=no",
                       "PasswordAuthentication=no", f"UserKnownHostsFile={private / 'known_hosts'}", "GlobalKnownHostsFile=/dev/null"):
            self.assertIn(needed, argv)
        self.assertIn("-a", argv)
        self.assertEqual(argv[argv.index("-i") + 1], str(private / f"m8-{run_id}.pem"))
        self.assertEqual(argv[-3:], [f"ubuntu@{ip}", "--", "sh -c 'echo $HOME; uname -m'"])
        joined = " ".join(argv)
        for forbidden in ("accept-new", "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=ask"):
            self.assertNotIn(forbidden, joined)
        self.assertNotIn("SSH_AUTH_SOCK", env)
        self.assertNotIn("AWS_SECRET_ACCESS_KEY", env)

    @unittest.skipUnless(shutil.which("ssh"), "ssh not installed")
    def test_ssh_argv_resolves_to_strict_pinned_options_in_openssh(self):
        """`ssh -G` only prints the resolved client config; it makes no connection."""
        self.root = self.root.parent / "evidence with space"
        run_id, summary = self.up()
        self.main(["ssh", "--run-id", run_id, "--arch", "amd64", "--", "id"])
        argv, env = self.ssh_calls[0]
        resolved = {}
        for line in subprocess.run(["ssh", "-G", *argv[1:]], env=env, capture_output=True, text=True, check=True, timeout=30).stdout.splitlines():
            key, _, value = line.partition(" ")
            resolved.setdefault(key, value)
        private = self.edir(run_id) / "private"
        expected = {"hostname": summary["instances"]["amd64"]["public_ip"], "user": "ubuntu", "stricthostkeychecking": "true",
                    "batchmode": "yes", "identitiesonly": "yes", "passwordauthentication": "no", "forwardagent": "no",
                    "userknownhostsfile": str(private / "known_hosts"), "globalknownhostsfile": "/dev/null",
                    "identityfile": str(private / f"m8-{run_id}.pem"), "clearallforwardings": "yes", "identityagent": "none"}
        for key, value in expected.items():
            self.assertEqual(resolved.get(key), value, key)

    def test_ssh_refuses_unsafe_or_missing_state(self):
        run_id, _ = self.up()
        private = self.edir(run_id) / "private"
        key, known = private / f"m8-{run_id}.pem", private / "known_hosts"
        key.chmod(0o644)
        self.assertEqual(self.main(["ssh", "--run-id", run_id, "--arch", "amd64", "--", "id"])[2].strip(), "refused: private_file_unsafe")
        key.chmod(0o600)
        known.write_text("203.0.113.99 ssh-ed25519 AAAA\n")
        self.assertIn("host_not_pinned", self.main(["ssh", "--run-id", run_id, "--arch", "amd64", "--", "id"])[2])
        known.unlink()
        self.assertIn("private_file_unsafe", self.main(["ssh", "--run-id", run_id, "--arch", "amd64", "--", "id"])[2])
        self.assertIn("ssh_command_required", self.main(["ssh", "--run-id", run_id, "--arch", "amd64"])[2])
        self.assertIn("state_not_found", self.main(["ssh", "--run-id", "0123456789ab", "--arch", "amd64", "--", "id"])[2])
        with self.assertRaises(SystemExit):
            self.main(["ssh", "--run-id", run_id, "--arch", "riscv", "--", "id"])
        self.assertEqual(self.ssh_calls, [])

    def test_ssh_refuses_a_run_that_is_not_ready(self):
        self.fake.console_mode = "none"
        run_id = self.plan()
        self.main(self.up_args(run_id) + ["--keep-on-failure"])
        self.assertIn("run_not_ready", self.main(["ssh", "--run-id", run_id, "--arch", "arm64", "--", "id"])[2])
        self.assertEqual(self.ssh_calls, [])


if __name__ == "__main__":
    unittest.main()
