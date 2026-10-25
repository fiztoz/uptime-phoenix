#!/usr/bin/env python3
"""Approval-gated AWS acceptance harness for the M8 probe host actions.

Subcommands: plan (default, offline) | up | down | sweep | ssh.  Python 3 standard
library plus the ``aws`` CLI (no boto3).  It creates two Ubuntu 24.04 hosts
(t4g.small arm64, t3.small amd64) in the default VPC, reachable on tcp/22 only from
one operator /32, each carrying a shutdown dead-man switch and terminate-on-halt.

Safety model (fail closed, nothing is guessed):
* ``plan`` makes no aws call.  It prints a fresh run id and stores the plan under
  ``tmp/m8-aws-<id>/``; ``up`` refuses any run id that ``plan`` did not print or whose
  parameters differ from the plan.
* Every aws call goes through ``AwsCli.run_aws``: an allow-list of operations, one
  ``--region``, ``--output json``, never ``shell=True``, redacted logs.
* ``down`` deletes only resources whose Project and RunId tags (and, for the key pair
  and security group, name) match, re-verified by describe immediately before each
  delete.  ``sweep`` is a read-only client and can never delete.
* SSH uses only host keys proven by the instance console output (``StrictHostKeyChecking=yes``).
"""
from __future__ import annotations

import argparse
import base64
import binascii
import dataclasses
import datetime as dt
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import struct
import subprocess
import sys
import time
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[1]
PROJECT = "uptime-phoenix-m8"
DEFAULT_REGION = "ap-southeast-1"
SUBCOMMANDS = ("plan", "up", "down", "sweep", "ssh")
SSH_USER = "ubuntu"
CANONICAL_OWNER = "099720109477"
ROOT_GIB = 16
UBUNTU_PARAM = "/aws/service/canonical/ubuntu/server/24.04/stable/current/{}/hvm/ebs-gp3/ami-id"
ARCHES = {  # arch -> instance type, EC2 Architecture value
    "arm64": {"type": "t4g.small", "ec2_arch": "arm64"},
    "amd64": {"type": "t3.small", "ec2_arch": "x86_64"},
}
# Rough on-demand list prices for ap-southeast-1 (USD/hour). NOT fetched; verify before approving.
PRICES = {"t4g.small": 0.0212, "t3.small": 0.0264, "gp3_gib_month": 0.096, "public_ipv4_hour": 0.005}

RUN_ID = re.compile(r"[0-9a-f]{12}\Z")
ACCOUNT = re.compile(r"[0-9]{12}\Z")
REGION = re.compile(r"[a-z]{2}(?:-[a-z]+)+-[0-9]\Z")
PROFILE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")
CIDR32 = re.compile(r"(?:[0-9]{1,3}\.){3}[0-9]{1,3}/32\Z")
IDS = {kind: re.compile(kind + r"-[0-9a-f]{8,17}\Z") for kind in ("i", "ami", "sg", "vpc", "subnet", "key")}
FP_BLOCK = ("-----BEGIN SSH HOST KEY FINGERPRINTS-----", "-----END SSH HOST KEY FINGERPRINTS-----")
KEY_BLOCK = ("-----BEGIN SSH HOST KEY KEYS-----", "-----END SSH HOST KEY KEYS-----")
FP_LINE = re.compile(r"\b[0-9]+ (SHA256:[A-Za-z0-9+/]{43})\s.*\((?:ED25519|ECDSA|RSA)\)\s*\Z")
KEY_LINE = re.compile(r"(?:\A|\s)(ssh-ed25519|ecdsa-sha2-nistp(?:256|384|521)|ssh-rsa) ([A-Za-z0-9+/]+={0,2})(?:\s|\Z)")
PEM = re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\Z)", re.S)
KEY_MATERIAL = re.compile(r'("KeyMaterial"\s*:\s*)"(?:[^"\\]|\\.)*"')

READ_ONLY_OPS = {("sts", "get-caller-identity"), ("ssm", "get-parameter")} | {
    ("ec2", op) for op in ("describe-vpcs", "describe-subnets", "describe-instance-type-offerings", "describe-images",
                           "describe-instances", "describe-instance-attribute", "describe-security-groups",
                           "describe-key-pairs", "get-console-output")}
WAITERS = {"instance-running", "instance-status-ok", "instance-terminated"}
MUTATING_OPS = {("ec2", op) for op in ("create-security-group", "authorize-security-group-ingress", "create-key-pair",
                                       "run-instances", "terminate-instances", "delete-key-pair", "delete-security-group")}
ALLOWED_OPS = READ_ONLY_OPS | MUTATING_OPS
WAIT_TIMEOUT = 1500


class HarnessError(Exception):
    """A fixed refusal reason code; never carries raw cloud output or secrets."""


class AwsError(HarnessError):
    """An aws CLI call that exited non-zero (stderr already redacted and truncated)."""


def need(condition: object, reason: str) -> None:
    """Fail closed with a fixed reason code."""
    if not condition:
        raise HarnessError(reason)


def field(obj: Any, key: str, kind: type = str, pattern: re.Pattern | None = None) -> Any:
    """Return obj[key] only if it has exactly the expected type (and shape)."""
    value = obj.get(key) if isinstance(obj, dict) else None
    need(isinstance(value, kind), f"unexpected_aws_response:{key}")
    need(pattern is None or pattern.fullmatch(value) is not None, f"unexpected_aws_response:{key}")
    return value


def redact(text: str) -> str:
    """Remove private-key material from any text bound for a log or an error."""
    return PEM.sub("[REDACTED PRIVATE KEY]", KEY_MATERIAL.sub(r'\1"[REDACTED]"', text))


# ----------------------------------------------------------------------------- validation

def validate_run_id(value: str) -> str:
    need(isinstance(value, str) and RUN_ID.fullmatch(value), "invalid_run_id")
    return value


def validate_account(value: str) -> str:
    need(isinstance(value, str) and ACCOUNT.fullmatch(value), "invalid_account_id")
    return value


def validate_region(value: str) -> str:
    need(isinstance(value, str) and REGION.fullmatch(value), "invalid_region")
    return value


def validate_profile(value: str | None) -> str | None:
    """An aws CLI profile name (default: none, i.e. the CLI's own credential chain)."""
    need(value is None or (isinstance(value, str) and PROFILE.fullmatch(value)), "invalid_profile")
    return value


def validate_cidr(value: str) -> str:
    """Exactly one routable-looking IPv4 host (/32). No default, no 0.0.0.0/0, no ranges."""
    need(isinstance(value, str) and CIDR32.fullmatch(value), "operator_cidr_must_be_ipv4_slash32")
    try:
        ip = ipaddress.IPv4Address(value[:-3])
    except ValueError:
        raise HarnessError("operator_cidr_invalid") from None
    need(not (ip.is_unspecified or ip.is_loopback or ip.is_multicast or ip.is_link_local or ip.is_reserved),
         "operator_cidr_not_a_unicast_host")
    return f"{ip}/32"


def validate_max_minutes(value: int) -> int:
    need(isinstance(value, int) and 15 <= value <= 480, "max_minutes_out_of_range")
    return value


# ----------------------------------------------------------------------------- aws choke point

Runner = Callable[[list[str], int], tuple[int, str, str]]


def subprocess_runner(argv: list[str], timeout: int) -> tuple[int, str, str]:
    """Run one aws CLI process without a shell; never inherits stdin."""
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, check=False, shell=False,
                              stdin=subprocess.DEVNULL, env=dict(os.environ, AWS_PAGER=""))
    except subprocess.TimeoutExpired:
        raise HarnessError("aws_cli_timeout") from None
    except OSError:
        raise HarnessError("aws_cli_unavailable") from None
    return done.returncode, done.stdout, done.stderr


def full_argv(region: str, args: list[str], profile: str | None = None) -> list[str]:
    """The exact argv for one aws call (shared by execution and by ``plan``)."""
    return ["aws", "--region", region, "--output", "json", "--no-cli-pager", *(["--profile", profile] if profile else []), *args]


class AwsCli:
    """Every aws call goes through ``run_aws``. Read-only clients refuse mutating operations."""

    def __init__(self, region: str, runner: Runner = subprocess_runner, log: Callable[[dict], None] | None = None,
                 read_only: bool = False, profile: str | None = None) -> None:
        self.region, self.runner, self.log, self.read_only = validate_region(region), runner, log, read_only
        self.profile = validate_profile(profile)

    def run_aws(self, args: list[str], timeout: int = 120) -> dict:
        """Run ``aws <args>`` and return the parsed JSON object ({} for empty output)."""
        need(len(args) >= 2 and all(isinstance(a, str) for a in args), "bad_aws_args")
        service, op = args[0], args[1]
        waiting = op == "wait" and service == "ec2" and len(args) > 2 and args[2] in WAITERS
        need(waiting or (service, op) in ALLOWED_OPS, f"aws_operation_not_allowed:{service} {op}")
        need(not (self.read_only and (service, op) in MUTATING_OPS), f"read_only_client:{service} {op}")
        need("--region" not in args and "--output" not in args and "--profile" not in args, "aws_global_option_forbidden")
        code, out, err = self.runner(full_argv(self.region, args, self.profile), timeout)
        shown_out, shown_err = redact(out), redact(err)
        if self.log is not None:
            self.log({"ts": dt.datetime.now(dt.timezone.utc).isoformat(), "argv": [redact(a) for a in args], "exit": code,
                      "stdout": shown_out[:2000], "stdout_sha256": hashlib.sha256(out.encode()).hexdigest(),
                      "stderr": shown_err[:1000]})
        if code != 0:
            raise AwsError(f"aws_failed:{service} {op}:{shown_err.strip()[:300]}")
        if not out.strip():
            return {}
        try:
            data = json.loads(out)
        except ValueError:
            raise HarnessError("aws_output_not_json") from None
        need(isinstance(data, dict), "aws_output_not_object")
        return data


# ----------------------------------------------------------------------------- private files

def ensure_private_dir(path: Path) -> Path:
    """Create or re-verify a real, self-owned directory and force mode 0700."""
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.mkdir(path, 0o700)
    except FileExistsError:
        pass
    st = os.lstat(path)
    need(stat.S_ISDIR(st.st_mode) and st.st_uid == os.geteuid(), "evidence_dir_unsafe")
    os.chmod(path, 0o700)
    return path


def write_private(path: Path, data: str | bytes, replace: bool = False) -> None:
    """Write a new 0600 file (O_EXCL|O_NOFOLLOW); ``replace`` swaps it in atomically."""
    target = path.with_name(path.name + ".tmp") if replace else path
    if replace:
        target.unlink(missing_ok=True)
    try:
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "wb") as handle:
            os.fchmod(handle.fileno(), 0o600)
            handle.write(data.encode() if isinstance(data, str) else data)
            handle.flush()
            os.fsync(handle.fileno())
        if replace:
            os.replace(target, path)
    except FileExistsError:
        raise HarnessError("refusing_to_overwrite_private_file") from None
    except OSError:
        raise HarnessError("private_file_write_failed") from None


def jsonl_logger(path: Path) -> Callable[[dict], None]:
    """Append-only 0600 JSONL evidence log for redacted aws calls."""
    def write(entry: dict) -> None:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "a", encoding="utf-8") as handle:
            os.fchmod(handle.fileno(), 0o600)
            handle.write(json.dumps(entry, sort_keys=True) + "\n")
    return write


def read_json_file(path: Path, reason: str) -> dict:
    """Read a regular, self-owned JSON object or refuse."""
    try:
        st = os.lstat(path)
        need(stat.S_ISREG(st.st_mode) and st.st_uid == os.geteuid(), reason)
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise HarnessError(reason) from None
    need(isinstance(data, dict), reason)
    return data


def save_state(edir: Path, state: dict) -> None:
    write_private(edir / "state.json", json.dumps(state, indent=2, sort_keys=True) + "\n", replace=True)


def load_state(edir: Path, run_id: str) -> dict:
    state = read_json_file(edir / "state.json", "state_not_found")
    need(state.get("run_id") == run_id and state.get("project") == PROJECT, "state_invalid")
    validate_region(field(state, "region"))
    validate_account(field(state, "account"))
    return state


# ----------------------------------------------------------------------------- command builders

def j(value: Any) -> str:
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def utc_iso(moment: dt.datetime) -> str:
    return moment.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def name_of(run_id: str) -> str:
    """Name shared by the run's key pair and security group."""
    return f"m8-{run_id}"


def tags_for(run_id: str, expires_at: str, name: str) -> list[dict]:
    return [{"Key": "Project", "Value": PROJECT}, {"Key": "RunId", "Value": run_id},
            {"Key": "ExpiresAt", "Value": expires_at}, {"Key": "Name", "Value": name}]


def tag_spec(resource_type: str, tags: list[dict]) -> dict:
    return {"ResourceType": resource_type, "Tags": tags}


def run_filter(run_id: str) -> str:
    return j([{"Name": "tag:Project", "Values": [PROJECT]}, {"Name": "tag:RunId", "Values": [run_id]}])


def project_filter() -> str:
    return j([{"Name": "tag:Project", "Values": [PROJECT]}])


def cmd_sts() -> list[str]:
    return ["sts", "get-caller-identity"]


def cmd_default_vpc() -> list[str]:
    return ["ec2", "describe-vpcs", "--filters", j([{"Name": "isDefault", "Values": ["true"]}])]


def cmd_default_subnets(vpc: str) -> list[str]:
    return ["ec2", "describe-subnets", "--filters",
            j([{"Name": "vpc-id", "Values": [vpc]}, {"Name": "default-for-az", "Values": ["true"]}])]


def cmd_offerings() -> list[str]:
    types = sorted(a["type"] for a in ARCHES.values())
    return ["ec2", "describe-instance-type-offerings", "--location-type", "availability-zone",
            "--filters", j([{"Name": "instance-type", "Values": types}])]


def cmd_ami_param(arch: str) -> list[str]:
    return ["ssm", "get-parameter", "--name", UBUNTU_PARAM.format(arch)]


def cmd_image(ami: str) -> list[str]:
    return ["ec2", "describe-images", "--image-ids", ami]


def cmd_create_sg(run_id: str, vpc: str, tags: list[dict]) -> list[str]:
    return ["ec2", "create-security-group", "--group-name", name_of(run_id), "--description",
            f"uptime-phoenix m8 acceptance {run_id}", "--vpc-id", vpc, "--tag-specifications",
            j([tag_spec("security-group", tags)])]


def cmd_ssh_ingress(sg: str, cidr: str) -> list[str]:
    rule = {"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22, "IpRanges": [{"CidrIp": cidr, "Description": "m8 operator ssh"}]}
    return ["ec2", "authorize-security-group-ingress", "--group-id", sg, "--ip-permissions", j([rule])]


def cmd_probe_ingress(sg: str, cidr: str, port: int) -> list[str]:
    """Opt-in: the probe's TLS runtime port, from the same operator /32 only."""
    rule = {"IpProtocol": "tcp", "FromPort": port, "ToPort": port, "IpRanges": [{"CidrIp": cidr, "Description": "m8 operator probe"}]}
    return ["ec2", "authorize-security-group-ingress", "--group-id", sg, "--ip-permissions", j([rule])]


def validate_probe_port(port: int) -> int:
    """0 means none; otherwise an unprivileged TCP port other than 22."""
    need(isinstance(port, int) and (port == 0 or 1024 <= port <= 65535), "invalid_probe_port")
    return port


def cmd_create_key(run_id: str, tags: list[dict]) -> list[str]:
    return ["ec2", "create-key-pair", "--key-name", name_of(run_id), "--key-type", "ed25519", "--key-format", "pem",
            "--tag-specifications", j([tag_spec("key-pair", tags)])]


def cmd_run_instance(run_id: str, arch: str, ami: str, root_device: str, subnet: str, sg: str,
                     user_data_path: str, tags: list[dict]) -> list[str]:
    disk = [{"DeviceName": root_device, "Ebs": {"VolumeSize": ROOT_GIB, "VolumeType": "gp3", "Encrypted": True,
                                                  "DeleteOnTermination": True}}]
    specs = [tag_spec(kind, tags) for kind in ("instance", "volume", "network-interface")]
    return ["ec2", "run-instances", "--count", "1", "--image-id", ami, "--instance-type", ARCHES[arch]["type"],
            "--key-name", name_of(run_id), "--security-group-ids", sg, "--subnet-id", subnet,
            "--instance-initiated-shutdown-behavior", "terminate",
            "--metadata-options", j({"HttpTokens": "required", "HttpEndpoint": "enabled", "HttpPutResponseHopLimit": 1}),
            "--block-device-mappings", j(disk), "--credit-specification", j({"CpuCredits": "standard"}),
            "--user-data", "file://" + user_data_path, "--client-token", f"m8-{run_id}-{arch}",
            "--tag-specifications", j(specs)]


def cmd_wait(waiter: str, ids: list[str]) -> list[str]:
    return ["ec2", "wait", waiter, "--instance-ids", *ids]


def cmd_describe_ids(ids: list[str]) -> list[str]:
    return ["ec2", "describe-instances", "--instance-ids", *ids]


def cmd_describe_run_instances(run_id: str) -> list[str]:
    return ["ec2", "describe-instances", "--filters", run_filter(run_id)]


def cmd_shutdown_attr(instance_id: str) -> list[str]:
    return ["ec2", "describe-instance-attribute", "--instance-id", instance_id, "--attribute", "instanceInitiatedShutdownBehavior"]


def cmd_console(instance_id: str) -> list[str]:
    return ["ec2", "get-console-output", "--instance-id", instance_id, "--latest"]


def cmd_terminate(instance_id: str) -> list[str]:
    return ["ec2", "terminate-instances", "--instance-ids", instance_id]


def cmd_find_keys(name: str) -> list[str]:
    return ["ec2", "describe-key-pairs", "--filters", j([{"Name": "key-name", "Values": [name]}])]


def cmd_describe_key(key_id: str) -> list[str]:
    return ["ec2", "describe-key-pairs", "--key-pair-ids", key_id]


def cmd_delete_key(key_id: str) -> list[str]:
    return ["ec2", "delete-key-pair", "--key-pair-id", key_id]


def cmd_find_sgs(name: str) -> list[str]:
    return ["ec2", "describe-security-groups", "--filters", j([{"Name": "group-name", "Values": [name]}])]


def cmd_describe_sg(sg: str) -> list[str]:
    return ["ec2", "describe-security-groups", "--group-ids", sg]


def cmd_delete_sg(sg: str) -> list[str]:
    return ["ec2", "delete-security-group", "--group-id", sg]


def user_data(run_id: str, max_minutes: int) -> str:
    """Dead-man switch: halt (= terminate, by shutdown behavior) after ``max_minutes``."""
    return ("#!/bin/sh\n# uptime-phoenix M8 acceptance dead-man switch, run " + run_id + "\nset -eu\n"
            f"shutdown -h +{max_minutes} 'uptime-phoenix-m8 {run_id} dead-man switch'\n")


def estimate(max_minutes: int) -> dict:
    """Approximate USD cost: both instances, 2x gp3 root and 2x public IPv4."""
    hourly = (PRICES["t4g.small"] + PRICES["t3.small"] + 2 * ROOT_GIB * PRICES["gp3_gib_month"] / 730
              + 2 * PRICES["public_ipv4_hour"])
    return {"hourly_usd": round(hourly, 4), "max_window_usd": round(hourly * max_minutes / 60, 3)}


# ----------------------------------------------------------------------------- response parsing

def tags_of(item: dict) -> dict[str, str]:
    raw = item.get("Tags", [])
    need(isinstance(raw, list) and all(isinstance(t, dict) and isinstance(t.get("Key"), str)
                                       and isinstance(t.get("Value"), str) for t in raw), "unexpected_aws_response:Tags")
    return {t["Key"]: t["Value"] for t in raw}


def owned(item: dict, run_id: str) -> bool:
    tags = tags_of(item)
    return tags.get("Project") == PROJECT and tags.get("RunId") == run_id


def instances_from(resp: dict) -> list[dict]:
    found = []
    for reservation in field(resp, "Reservations", list):
        for item in field(reservation, "Instances", list):
            field(item, "InstanceId", str, IDS["i"])
            field(field(item, "State", dict), "Name")
            found.append(item)
    return found


def items_from(resp: dict, key: str, id_key: str, kind: str) -> list[dict]:
    items = field(resp, key, list)
    for item in items:
        field(item, id_key, str, IDS[kind])
    return items


def block_lines(text: str, markers: tuple[str, str]) -> list[str]:
    """Lines of the last complete BEGIN/END block (markers may carry a console prefix)."""
    best: list[str] = []
    current: list[str] | None = None
    for line in text.splitlines():
        if markers[0] in line:
            current = []
        elif markers[1] in line and current is not None:
            best, current = current, None
        elif current is not None:
            current.append(line)
    return best


def console_fingerprints(text: str) -> set[str]:
    """SHA256 host key fingerprints printed by cloud-init (MD5 or malformed lines are ignored)."""
    return {m.group(1) for line in block_lines(text, FP_BLOCK) if (m := FP_LINE.search(line))}


def key_fingerprint(key_type: str, b64: str) -> str:
    try:
        blob = base64.b64decode(b64, validate=True)
    except (binascii.Error, ValueError):
        raise HarnessError("host_key_malformed") from None
    need(len(blob) > 4, "host_key_malformed")
    (length,) = struct.unpack(">I", blob[:4])
    need(blob[4:4 + length] == key_type.encode(), "host_key_type_mismatch")
    return "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")


def public_keys(lines: list[str]) -> list[tuple[str, str]]:
    """Well-formed (type, base64) host keys found in the given lines."""
    keys = []
    for line in lines:
        match = KEY_LINE.search(line)
        if match:
            try:
                key_fingerprint(*match.groups())
            except HarnessError:
                continue
            if match.groups() not in keys:
                keys.append(match.groups())
    return keys


def verified_keys(keys: list[tuple[str, str]], fingerprints: set[str]) -> list[tuple[str, str]]:
    """Only keys whose SHA256 fingerprint appears in the console fingerprint block."""
    return [k for k in keys if key_fingerprint(*k) in fingerprints]


# ----------------------------------------------------------------------------- dependencies

def default_keyscan(host: str) -> str:
    """Candidate keys only; every key is re-verified against the console fingerprints."""
    try:
        done = subprocess.run(["ssh-keyscan", "-T", "10", "-t", "ed25519,ecdsa,rsa", host], capture_output=True,
                              text=True, timeout=40, check=False, shell=False, stdin=subprocess.DEVNULL)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return done.stdout


def default_ssh_runner(argv: list[str], env: dict[str, str]) -> int:
    return subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, check=False, shell=False).returncode


@dataclasses.dataclass
class Deps:
    """Injectable edges: aws runner, clock, sleep, ssh-keyscan, ssh and the evidence root."""
    root: Path
    runner: Runner = subprocess_runner
    sleep: Callable[[float], None] = time.sleep
    now: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)
    keyscan: Callable[[str], str] = default_keyscan
    ssh_runner: Callable[[list[str], dict[str, str]], int] = default_ssh_runner
    profile: str | None = None

    def aws(self, region: str, edir: Path | None = None, read_only: bool = False) -> AwsCli:
        log = jsonl_logger(edir / "aws-calls.jsonl") if edir is not None else None
        return AwsCli(region, self.runner, log, read_only, self.profile)

    def hint(self, text: str) -> str:
        """A copy-paste follow-up command that carries the same profile."""
        return f"python3 scripts/m8_aws_acceptance.py {text}" + (f" --profile {self.profile}" if self.profile else "")

    def run_dir(self, run_id: str) -> Path:
        return self.root / f"m8-aws-{validate_run_id(run_id)}"


# ----------------------------------------------------------------------------- plan

def plan_steps(run_id: str, cidr: str, max_minutes: int, probe_port: int = 0) -> list[tuple[str, list[str]]]:
    """The ordered ``up`` calls with placeholders for discovered ids (same builders as ``up``)."""
    expires = "<NOW+" + str(max_minutes) + "m UTC>"
    steps = [("verify caller account", cmd_sts()), ("discover default VPC", cmd_default_vpc()),
             ("discover default subnets", cmd_default_subnets("<VPC_ID>")),
             ("pick an AZ offering both instance types", cmd_offerings())]
    steps += [(f"latest Ubuntu 24.04 {a} AMI (SSM public parameter)", cmd_ami_param(a)) for a in ARCHES]
    steps += [(f"verify {a} AMI owner/architecture", cmd_image(f"<AMI_{a.upper()}>")) for a in ARCHES]
    steps += [("create security group", cmd_create_sg(run_id, "<VPC_ID>", tags_for(run_id, expires, name_of(run_id)))),
              ("allow tcp/22 from the operator /32 only", cmd_ssh_ingress("<SG_ID>", cidr))]
    if probe_port:
        steps += [(f"allow tcp/{probe_port} (probe runtime) from the operator /32 only", cmd_probe_ingress("<SG_ID>", cidr, probe_port))]
    steps += [("create ED25519 key pair (private key saved 0600, never logged)",
               cmd_create_key(run_id, tags_for(run_id, expires, name_of(run_id))))]
    steps += [(f"launch {a} instance", cmd_run_instance(run_id, a, f"<AMI_{a.upper()}>", "<ROOT_DEVICE>", "<SUBNET_ID>", "<SG_ID>",
                                                        "<EVIDENCE>/user-data.sh", tags_for(run_id, expires, f"{name_of(run_id)}-{a}")))
              for a in ARCHES]
    steps += [("wait running", cmd_wait("instance-running", ["<INSTANCE_IDS>"])),
              ("wait status checks OK", cmd_wait("instance-status-ok", ["<INSTANCE_IDS>"])),
              ("verify tags/IMDSv2/IPs", cmd_describe_ids(["<INSTANCE_IDS>"]))]
    steps += [(f"verify {a} terminate-on-shutdown", cmd_shutdown_attr(f"<INSTANCE_{a.upper()}>")) for a in ARCHES]
    steps += [(f"read {a} console host key fingerprints (pinning)", cmd_console(f"<INSTANCE_{a.upper()}>")) for a in ARCHES]
    return steps


def teardown_steps(run_id: str) -> list[tuple[str, list[str]]]:
    """The ordered ``down`` calls (same builders as ``teardown``); each delete is preceded by a by-id describe."""
    name = name_of(run_id)
    return [("find the run's instances (client-side Project+RunId re-check)", cmd_describe_run_instances(run_id)),
            ("re-verify each instance by id", cmd_describe_ids(["<INSTANCE_ID>"])),
            ("terminate it", cmd_terminate("<INSTANCE_ID>")),
            ("wait until terminated (the SG is deleted only after this)", cmd_wait("instance-terminated", ["<INSTANCE_IDS>"])),
            ("find the key pair by name (client-side name+tag re-check)", cmd_find_keys(name)),
            ("re-verify the key pair by id", cmd_describe_key("<KEY_PAIR_ID>")), ("delete it", cmd_delete_key("<KEY_PAIR_ID>")),
            ("find the security group by name (client-side name+tag re-check)", cmd_find_sgs(name)),
            ("re-verify the security group by id", cmd_describe_sg("<SG_ID>")), ("delete it", cmd_delete_sg("<SG_ID>"))]


def render_plan(run_id: str, a: argparse.Namespace, edir: Path, d: Deps) -> list[str]:
    cost = estimate(a.max_minutes)
    out = ["M8 AWS acceptance PLAN (offline: no aws command was run)", "",
           f"run id          : {run_id}", f"region          : {a.region}", f"aws profile     : {a.profile or '(none: default credential chain)'}",
           f"operator ssh    : {a.operator_cidr} (tcp/22" + (f" and tcp/{a.probe_port}" if a.probe_port else "") + " only)",
           f"expected account: {a.expect_account or '(none yet: pass --expect-account <12 digits> to up)'}",
           f"evidence dir    : {edir}", f"dead-man switch : shutdown -h +{a.max_minutes} (terminate on halt)", "",
           "Resources (all tagged Project=%s RunId=%s ExpiresAt=<UTC>):" % (PROJECT, run_id),
           f"  security group {name_of(run_id)} (ingress tcp/22" + (f" and tcp/{a.probe_port}" if a.probe_port else "") + f" from {a.operator_cidr} only)",
           f"  ED25519 key pair {name_of(run_id)}"]
    out += [f"  instance {x['type']} ({arch}) Ubuntu 24.04, {ROOT_GIB} GiB encrypted gp3, IMDSv2 required, standard CPU credits"
            for arch, x in ARCHES.items()]
    out += ["", "Estimated cost (rough ap-southeast-1 list prices, NOT fetched; verify before approving):",
            f"  ~${cost['hourly_usd']}/hour for both hosts, ~${cost['max_window_usd']} for the {a.max_minutes} minute window", "",
            "Commands, in order (placeholders in <> are discovered by up):"]
    out += [f"  # {note}\n  {shlex.join(full_argv(a.region, argv, a.profile))}" for note, argv in plan_steps(run_id, a.operator_cidr, a.max_minutes, a.probe_port)]
    out += ["", "user-data (scheduled at first boot):"] + ["  " + line for line in user_data(run_id, a.max_minutes).splitlines()]
    out += ["", "Teardown plan (%s): only resources tagged Project=%s and RunId=%s are touched, each re-verified by "
            "describe before deletion:" % (d.hint(f"down --run-id {run_id} --region {a.region}"), PROJECT, run_id)]
    out += [f"  # {note}\n  {shlex.join(full_argv(a.region, argv, a.profile))}" for note, argv in teardown_steps(run_id)]
    out += ["", "To approve, run exactly:",
            "  " + d.hint(f"up --approve-run-id {run_id} --region {a.region} --operator-cidr {a.operator_cidr} "
                          f"--max-minutes {a.max_minutes} --expect-account {a.expect_account or '<12 digits>'}")]
    return out


def do_plan(d: Deps, a: argparse.Namespace, remote: list[str]) -> int:
    """Offline: validate, mint a fresh run id, persist the plan locally, print it."""
    validate_region(a.region)
    validate_profile(a.profile)
    validate_cidr(a.operator_cidr)
    validate_max_minutes(a.max_minutes)
    validate_probe_port(a.probe_port)
    if a.expect_account:
        validate_account(a.expect_account)
    run_id = secrets.token_hex(6)
    edir = ensure_private_dir(d.run_dir(run_id))
    write_private(edir / "plan.json", json.dumps({"run_id": run_id, "region": a.region, "operator_cidr": a.operator_cidr,
                                                  "max_minutes": a.max_minutes, "probe_port": a.probe_port, "planned_at": utc_iso(d.now())},
                                                 indent=2, sort_keys=True) + "\n")
    print("\n".join(render_plan(run_id, a, edir, d)))
    return 0


# ----------------------------------------------------------------------------- up

def discover_network(aws: AwsCli) -> tuple[str, str, str]:
    vpcs = items_from(aws.run_aws(cmd_default_vpc()), "Vpcs", "VpcId", "vpc")
    need(len(vpcs) == 1, "default_vpc_not_unique")
    vpc = vpcs[0]["VpcId"]
    subnets = items_from(aws.run_aws(cmd_default_subnets(vpc)), "Subnets", "SubnetId", "subnet")
    offered: dict[str, set[str]] = {}
    for item in field(aws.run_aws(cmd_offerings()), "InstanceTypeOfferings", list):
        offered.setdefault(field(item, "Location"), set()).add(field(item, "InstanceType"))
    wanted = {x["type"] for x in ARCHES.values()}
    usable = sorted((field(s, "AvailabilityZone"), s["SubnetId"]) for s in subnets
                    if s.get("VpcId") == vpc and s.get("State") == "available" and s.get("MapPublicIpOnLaunch") is True
                    and s.get("DefaultForAz") is True and wanted <= offered.get(s.get("AvailabilityZone"), set()))
    need(usable, "no_default_subnet_offers_both_instance_types")
    return vpc, usable[0][1], usable[0][0]


def discover_amis(aws: AwsCli) -> dict[str, dict]:
    params = {arch: field(field(aws.run_aws(cmd_ami_param(arch)), "Parameter", dict), "Value", str, IDS["ami"]) for arch in ARCHES}
    found = {}
    for arch, spec in ARCHES.items():
        ami = params[arch]
        images = field(aws.run_aws(cmd_image(ami)), "Images", list)
        need(len(images) == 1 and images[0].get("ImageId") == ami, "ami_not_unique")
        image = images[0]
        need(image.get("OwnerId") == CANONICAL_OWNER and image.get("Architecture") == spec["ec2_arch"]
             and image.get("State") == "available", "ami_not_official_canonical")
        found[arch] = {"ami": ami, "root_device": field(image, "RootDeviceName")}
    return found


def save_key(private: Path, run_id: str, response: dict) -> Path:
    """Persist the private key at 0600 (O_EXCL) and verify its mode; the key is never logged."""
    material = field(response, "KeyMaterial")
    need(re.search(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----", material) and "-----END" in material, "key_material_invalid")
    path = private / f"{name_of(run_id)}.pem"
    write_private(path, material if material.endswith("\n") else material + "\n")
    need(stat.S_IMODE(os.lstat(path).st_mode) == 0o600, "key_mode_invalid")
    response.pop("KeyMaterial", None)
    return path


def verify_instances(aws: AwsCli, run_id: str, sg: str, ids: dict[str, str]) -> dict[str, dict]:
    items = {i["InstanceId"]: i for i in instances_from(aws.run_aws(cmd_describe_ids(list(ids.values()))))}
    need(set(items) == set(ids.values()), "instances_mismatch")
    result = {}
    for arch, iid in ids.items():
        item = items[iid]
        groups = {g.get("GroupId") for g in field(item, "SecurityGroups", list)}
        need(owned(item, run_id) and item["State"]["Name"] == "running" and item.get("InstanceType") == ARCHES[arch]["type"]
             and item.get("KeyName") == name_of(run_id) and groups == {sg}
             and field(item, "MetadataOptions", dict).get("HttpTokens") == "required", "instance_verification_failed")
        try:
            ip = str(ipaddress.IPv4Address(field(item, "PublicIpAddress")))
        except ValueError:
            raise HarnessError("instance_public_ip_invalid") from None
        shutdown = field(aws.run_aws(cmd_shutdown_attr(iid)), "InstanceInitiatedShutdownBehavior", dict)
        need(shutdown.get("Value") == "terminate", "shutdown_behavior_not_terminate")
        result[arch] = {"instance_id": iid, "public_ip": ip, "private_ip": item.get("PrivateIpAddress")}
    return result


def pin_host_keys(d: Deps, aws: AwsCli, instance_id: str, ip: str) -> tuple[list[str], list[tuple[str, str]]]:
    """Host keys proven by the console fingerprints (out-of-band via the AWS API); refuse otherwise."""
    for _ in range(40):
        resp = aws.run_aws(cmd_console(instance_id))
        need(field(resp, "InstanceId") == instance_id, "console_instance_mismatch")
        text = resp.get("Output")
        fingerprints = console_fingerprints(text) if isinstance(text, str) else set()
        if fingerprints:
            break
        d.sleep(15)
    else:
        raise HarnessError("host_key_fingerprints_unavailable")
    pinned = verified_keys(public_keys(block_lines(text, KEY_BLOCK)), fingerprints)
    if not pinned:  # keys block absent: candidates from ssh-keyscan are accepted only if they match the console
        pinned = verified_keys(public_keys(d.keyscan(ip).splitlines()), fingerprints)
    need(pinned, "host_key_unverifiable")
    return sorted(fingerprints), pinned


def do_up(d: Deps, a: argparse.Namespace, remote: list[str]) -> int:
    """Create and verify both hosts. On failure tears the run down unless --keep-on-failure."""
    run_id = validate_run_id(a.approve_run_id)
    region, cidr = validate_region(a.region), validate_cidr(a.operator_cidr)
    minutes, expect = validate_max_minutes(a.max_minutes), validate_account(a.expect_account)
    edir = d.run_dir(run_id)
    plan = read_json_file(edir / "plan.json", "run_id_not_printed_by_plan")
    probe_port = validate_probe_port(a.probe_port)
    need(plan.get("run_id") == run_id and (plan.get("region"), plan.get("operator_cidr"), plan.get("max_minutes"), plan.get("probe_port", 0))
         == (region, cidr, minutes, probe_port), "plan_mismatch")
    need(not (edir / "state.json").exists(), "run_id_already_used")
    private = ensure_private_dir(edir / "private")
    aws = d.aws(region, edir)
    account = field(aws.run_aws(cmd_sts()), "Account", str, ACCOUNT)
    need(account == expect, "account_mismatch")
    created = d.now()
    expires = utc_iso(created + dt.timedelta(minutes=minutes))
    state: dict[str, Any] = {"project": PROJECT, "run_id": run_id, "region": region, "account": account, "operator_cidr": cidr,
                             "max_minutes": minutes, "created_at": utc_iso(created), "expires_at": expires, "status": "creating",
                             "instances": {}}
    save_state(edir, state)
    started = False
    try:
        vpc, subnet, zone = discover_network(aws)
        amis = discover_amis(aws)
        state.update(vpc_id=vpc, subnet_id=subnet, availability_zone=zone)
        write_private(edir / "user-data.sh", user_data(run_id, minutes))
        need(not os.path.lexists(private / f"{name_of(run_id)}.pem"), "key_file_already_exists")
        started = True  # from here on resources may exist, even if a call fails midway
        sg = field(aws.run_aws(cmd_create_sg(run_id, vpc, tags_for(run_id, expires, name_of(run_id)))), "GroupId", str, IDS["sg"])
        state["security_group_id"] = sg
        save_state(edir, state)
        aws.run_aws(cmd_ssh_ingress(sg, cidr))
        if probe_port:
            aws.run_aws(cmd_probe_ingress(sg, cidr, probe_port))
        key = aws.run_aws(cmd_create_key(run_id, tags_for(run_id, expires, name_of(run_id))))
        key_path = save_key(private, run_id, key)
        state.update(key_pair_id=field(key, "KeyPairId", str, IDS["key"]), key_name=name_of(run_id),
                     key_file=str(key_path.relative_to(edir)))
        save_state(edir, state)
        ids = {}
        for arch in ARCHES:
            tags = tags_for(run_id, expires, f"{name_of(run_id)}-{arch}")
            resp = aws.run_aws(cmd_run_instance(run_id, arch, amis[arch]["ami"], amis[arch]["root_device"], subnet, sg,
                                                str(edir / "user-data.sh"), tags))
            launched = field(resp, "Instances", list)
            need(len(launched) == 1, "run_instances_count_unexpected")
            ids[arch] = field(launched[0], "InstanceId", str, IDS["i"])
            state["instances"][arch] = {"instance_id": ids[arch], "ami": amis[arch]["ami"], "instance_type": ARCHES[arch]["type"]}
            save_state(edir, state)
        aws.run_aws(cmd_wait("instance-running", list(ids.values())), WAIT_TIMEOUT)
        aws.run_aws(cmd_wait("instance-status-ok", list(ids.values())), WAIT_TIMEOUT)
        hosts = verify_instances(aws, run_id, sg, ids)
        lines = []
        for arch, host in hosts.items():
            fingerprints, pinned = pin_host_keys(d, aws, host["instance_id"], host["public_ip"])
            state["instances"][arch].update(host, host_key_fingerprints=fingerprints)
            lines += [f"{host['public_ip']} {ktype} {b64}" for ktype, b64 in pinned]
        write_private(private / "known_hosts", "\n".join(lines) + "\n")
        state["status"] = "ready"
        save_state(edir, state)
    except BaseException as exc:
        state["status"] = "failed"
        state["failure"] = str(exc) if isinstance(exc, HarnessError) else type(exc).__name__
        save_state(edir, state)
        if started and not a.keep_on_failure:
            try:
                teardown(d, aws, run_id)
                print("up failed: the run's resources were torn down", file=sys.stderr)
            except Exception:
                print(f"up failed AND teardown failed: run `{d.hint(f'down --run-id {run_id} --region {region}')}` now", file=sys.stderr)
        raise
    print(json.dumps({"run_id": run_id, "account": account, "region": region, "expires_at": expires, "evidence_dir": str(edir),
                      "instances": state["instances"], "ssh": f"python3 scripts/m8_aws_acceptance.py ssh --run-id {run_id} --arch arm64 -- uname -a",
                      "teardown": d.hint(f"down --run-id {run_id} --region {region}")}, indent=2, sort_keys=True))
    return 0


# ----------------------------------------------------------------------------- down / sweep

def verified_one(resp: dict, key: str, id_key: str, kind: str, ident: str, run_id: str, name_key: str, name: str) -> None:
    """The fresh describe of exactly one id must still be this run's resource (tags and name)."""
    items = items_from(resp, key, id_key, kind)
    need(len(items) == 1 and items[0][id_key] == ident and items[0].get(name_key) == name and owned(items[0], run_id),
         "ownership_unverified")


def delete_retrying(d: Deps, aws: AwsCli, args: list[str]) -> None:
    """Delete once; retry only DependencyViolation (ENIs can linger briefly after termination)."""
    for attempt in range(6):
        try:
            aws.run_aws(args)
            return
        except AwsError as exc:
            if "DependencyViolation" not in str(exc) or attempt == 5:
                raise
            d.sleep(10)


def teardown(d: Deps, aws: AwsCli, run_id: str) -> dict:
    """Terminate, then delete key pair and SG, touching only this run's re-verified resources."""
    name = name_of(run_id)
    report: dict[str, Any] = {"run_id": run_id, "terminated": [], "key_pairs_deleted": [], "security_groups_deleted": [],
                              "foreign_skipped": 0}
    found = instances_from(aws.run_aws(cmd_describe_run_instances(run_id)))
    mine = [i["InstanceId"] for i in found if owned(i, run_id)]
    report["foreign_skipped"] += len(found) - len(mine)
    live = []
    for iid in mine:
        fresh = instances_from(aws.run_aws(cmd_describe_ids([iid])))
        need(len(fresh) == 1 and fresh[0]["InstanceId"] == iid and owned(fresh[0], run_id), "ownership_unverified")
        state_name = fresh[0]["State"]["Name"]
        if state_name == "terminated":
            continue
        if state_name != "shutting-down":
            done = field(aws.run_aws(cmd_terminate(iid)), "TerminatingInstances", list)
            need(any(t.get("InstanceId") == iid for t in done), "terminate_unconfirmed")
            report["terminated"].append(iid)
        live.append(iid)
    if live:
        aws.run_aws(cmd_wait("instance-terminated", live), WAIT_TIMEOUT)
    for report_key, list_key, id_key, kind, name_key, find, by_id, delete in (
            ("key_pairs_deleted", "KeyPairs", "KeyPairId", "key", "KeyName", cmd_find_keys, cmd_describe_key, cmd_delete_key),
            ("security_groups_deleted", "SecurityGroups", "GroupId", "sg", "GroupName", cmd_find_sgs, cmd_describe_sg, cmd_delete_sg)):
        items = items_from(aws.run_aws(find(name)), list_key, id_key, kind)
        mine_ids = [i[id_key] for i in items if i.get(name_key) == name and owned(i, run_id)]
        report["foreign_skipped"] += len(items) - len(mine_ids)
        for ident in mine_ids:
            verified_one(aws.run_aws(by_id(ident)), list_key, id_key, kind, ident, run_id, name_key, name)
            delete_retrying(d, aws, delete(ident))
            report[report_key].append(ident)
    return report


def check_account(aws: AwsCli, expected: str | None) -> str:
    account = field(aws.run_aws(cmd_sts()), "Account", str, ACCOUNT)
    need(expected is None or account == expected, "account_mismatch")
    return account


def do_down(d: Deps, a: argparse.Namespace, remote: list[str]) -> int:
    """Idempotent teardown of one run id; refuses an unknown account or a region/account mismatch."""
    run_id, region = validate_run_id(a.run_id), validate_region(a.region)
    expect = validate_account(a.expect_account) if a.expect_account else None
    edir = d.run_dir(run_id)
    state = None
    if (edir / "state.json").exists():
        state = load_state(edir, run_id)
        need(state["region"] == region, "region_mismatch")
        need(expect is None or expect == state["account"], "account_mismatch")
        expect = state["account"]
    need(expect is not None, "account_unknown_pass_expect_account")
    aws = d.aws(region, ensure_private_dir(edir))
    check_account(aws, expect)
    report = teardown(d, aws, run_id)
    if state is not None:
        state["status"] = "torn_down"
        save_state(edir, state)
        (edir / "private" / f"{name_of(run_id)}.pem").unlink(missing_ok=True)  # the key pair no longer exists
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


def do_sweep(d: Deps, a: argparse.Namespace, remote: list[str]) -> int:
    """List (never delete) tagged resources whose ExpiresAt has passed. Exit 3 when any are found."""
    region = validate_region(a.region)
    aws = d.aws(region, read_only=True)
    account = check_account(aws, validate_account(a.expect_account) if a.expect_account else None)
    now, rows = d.now(), []
    sources = [("instance", instances_from(aws.run_aws(["ec2", "describe-instances", "--filters", project_filter()])), "InstanceId"),
               ("security-group", items_from(aws.run_aws(["ec2", "describe-security-groups", "--filters", project_filter()]),
                                             "SecurityGroups", "GroupId", "sg"), "GroupId"),
               ("key-pair", items_from(aws.run_aws(["ec2", "describe-key-pairs", "--filters", project_filter()]),
                                       "KeyPairs", "KeyPairId", "key"), "KeyPairId")]
    for kind, items, id_key in sources:
        for item in items:
            tags = tags_of(item)
            if tags.get("Project") != PROJECT or (kind == "instance" and item["State"]["Name"] == "terminated"):
                continue
            try:
                lapsed = dt.datetime.strptime(tags["ExpiresAt"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc) <= now
                status = "expired" if lapsed else "active"
            except (KeyError, ValueError):
                status = "unparseable_expires_at"
            run_id = tags.get("RunId", "")
            rows.append({"kind": kind, "id": item[id_key], "run_id": run_id if RUN_ID.fullmatch(run_id) else None,
                         "expires_at": tags.get("ExpiresAt"), "status": status})
    expired = [r for r in rows if r["status"] != "active"]
    for row in expired:
        row["teardown"] = d.hint(f"down --run-id {row['run_id']} --region {region}") if row["run_id"] else None
    print(json.dumps({"account": account, "region": region, "checked_at": utc_iso(now), "active": len(rows) - len(expired),
                      "expired": expired}, indent=2, sort_keys=True))
    return 3 if expired else 0


# ----------------------------------------------------------------------------- ssh

SSH_OPTIONS = ["-F", "/dev/null", "-a", "-x", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
               "-o", "GlobalKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
               "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "ForwardAgent=no",
               "-o", "ClearAllForwardings=yes", "-o", "UpdateHostKeys=no", "-o", "VerifyHostKeyDNS=no",
               "-o", "HashKnownHosts=no", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15"]


def private_file_ok(path: Path) -> bool:
    """A real, self-owned 0600 file (symlinks and looser modes are refused)."""
    try:
        st = os.lstat(path)
    except OSError:
        return False
    return stat.S_ISREG(st.st_mode) and stat.S_IMODE(st.st_mode) == 0o600 and st.st_uid == os.geteuid()


def do_ssh(d: Deps, a: argparse.Namespace, remote: list[str]) -> int:
    """Run one command on a host using only the pinned known_hosts and the run's key."""
    run_id = validate_run_id(a.run_id)
    need(remote and all(isinstance(x, str) for x in remote), "ssh_command_required")
    edir = d.run_dir(run_id)
    state = load_state(edir, run_id)
    need(state.get("status") == "ready", "run_not_ready")
    host = field(field(state, "instances", dict), a.arch, dict)
    try:
        ip = str(ipaddress.IPv4Address(field(host, "public_ip")))
    except ValueError:
        raise HarnessError("state_invalid") from None
    key, known = edir / "private" / f"{name_of(run_id)}.pem", edir / "private" / "known_hosts"
    need(private_file_ok(key) and private_file_ok(known), "private_file_unsafe")
    need(any(line.startswith(ip + " ") for line in known.read_text(encoding="utf-8").splitlines()), "host_not_pinned")
    need(not re.search(r'["\\\x00-\x1f]', str(known)), "evidence_path_unsafe_for_ssh")
    known_option = f'"{known}"' if " " in str(known) else str(known)  # ssh -o values split on whitespace unless quoted
    argv = ["ssh", *SSH_OPTIONS, "-o", f"UserKnownHostsFile={known_option}", "-i", str(key), f"{SSH_USER}@{ip}", "--", shlex.join(remote)]
    env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "LANG", "TERM")}  # no SSH_AUTH_SOCK
    return d.ssh_runner(argv, env)


# ----------------------------------------------------------------------------- cli

def build_parser() -> argparse.ArgumentParser:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--region", default=DEFAULT_REGION)
    common.add_argument("--profile", default=None, help="aws CLI profile passed to every aws call (default: none)")
    common.add_argument("--evidence-root", default=str(ROOT / "tmp"), help="parent of m8-aws-<id>/ (default: repo tmp/)")
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)
    plan = sub.add_parser("plan", parents=[common], help="offline: print resources, commands, cost, teardown (default)")
    plan.add_argument("--operator-cidr", required=True, help="your IPv4 as a /32; the only source allowed on tcp/22")
    plan.add_argument("--max-minutes", type=int, default=120)
    plan.add_argument("--probe-port", type=int, default=0, help="also allow this probe TLS port from the operator /32 (default: none)")
    plan.add_argument("--expect-account")
    up = sub.add_parser("up", parents=[common], help="create the two hosts (needs the run id printed by plan)")
    up.add_argument("--approve-run-id", required=True)
    up.add_argument("--operator-cidr", required=True)
    up.add_argument("--expect-account", required=True)
    up.add_argument("--max-minutes", type=int, default=120)
    up.add_argument("--probe-port", type=int, default=0, help="must match plan")
    up.add_argument("--keep-on-failure", action="store_true", help="do not auto-teardown when up fails")
    down = sub.add_parser("down", parents=[common], help="terminate and delete one run's resources")
    down.add_argument("--run-id", required=True)
    down.add_argument("--expect-account")
    sweep = sub.add_parser("sweep", parents=[common], help="list (never delete) expired tagged resources")
    sweep.add_argument("--expect-account")
    ssh = sub.add_parser("ssh", parents=[common], help="ssh <cmd> after `--`, with pinned host keys")
    ssh.add_argument("--run-id", required=True)
    ssh.add_argument("--arch", required=True, choices=sorted(ARCHES))
    return parser


HANDLERS = {"plan": do_plan, "up": do_up, "down": do_down, "sweep": do_sweep, "ssh": do_ssh}


def main(argv: list[str] | None = None, *, runner: Runner | None = None, sleep: Callable[[float], None] | None = None,
         now: Callable[[], dt.datetime] | None = None, keyscan: Callable[[str], str] | None = None,
         ssh_runner: Callable[[list[str], dict[str, str]], int] | None = None) -> int:
    """Entry point; the keyword arguments inject the aws runner and other edges for offline tests."""
    args = list(sys.argv[1:] if argv is None else argv)
    remote: list[str] = []
    if "--" in args:
        split = args.index("--")
        args, remote = args[:split], args[split + 1:]
    if not args or (args[0] not in SUBCOMMANDS and args[0] not in ("-h", "--help")):
        args.insert(0, "plan")
    parsed = build_parser().parse_args(args)
    overrides = {k: v for k, v in dict(runner=runner, sleep=sleep, now=now, keyscan=keyscan, ssh_runner=ssh_runner).items() if v is not None}
    deps = Deps(root=Path(parsed.evidence_root).absolute(), profile=parsed.profile, **overrides)
    try:
        need(not remote or parsed.cmd == "ssh", "unexpected_double_dash")
        return HANDLERS[parsed.cmd](deps, parsed, remote)
    except HarnessError as exc:
        print(f"refused: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
