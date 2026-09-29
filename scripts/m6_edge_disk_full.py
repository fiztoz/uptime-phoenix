#!/usr/bin/env python3
"""Run edge ENOSPC and abrupt-commit-crash tests on a no-network 32 MiB tmpfs.

Requires cached mariadb:11 solely as a Linux userspace image. No MariaDB server,
external DSN, host filesystem quota, or privileged Docker mount is used.
"""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import uuid

ROOT = Path(__file__).resolve().parents[1]
DISK_TEST = "TestEdgeDiskFullCriticalCommit"
CRASH_TEST = "TestEdgeCheckCrashAroundCommit"


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], text=True, capture_output=True, check=False)
    if check and result.returncode:
        raise RuntimeError(f"docker {args[0]} failed: {result.stderr[-1200:]}")
    return result


def main():
    host = os.environ.get("DOCKER_HOST") or docker(
        "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"
    ).stdout.strip()
    if not host.startswith("unix://"):
        raise RuntimeError("disk-full acceptance requires a local Unix Docker socket")
    docker("image", "inspect", "mariadb:11")  # Fail closed; never pull an image.
    arch = docker("info", "--format", "{{.Architecture}}").stdout.strip()
    go_arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(arch)
    if go_arch is None:
        raise RuntimeError(f"unsupported isolated Linux architecture: {arch}")
    with tempfile.TemporaryDirectory(prefix="phoenix-m6-enospc-") as tmp:
        binary = Path(tmp) / "edge-test"
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=go_arch,
                   GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
        build = subprocess.run(
            ["go", "test", "-c", "-o", str(binary), "./internal/adapters/repository/edge"],
            cwd=ROOT, env=env, text=True, capture_output=True, check=False
        )
        if build.returncode:
            raise RuntimeError(f"offline Go test build failed: {build.stderr[-1200:]}")
        name = "phoenix-m6-enospc-" + uuid.uuid4().hex[:10]
        docker("run", "-d", "--name", name, "--network", "none", "--cpus", "2",
               "--memory", "512m", "--tmpfs", "/data:rw,size=32m,mode=1777",
               "--entrypoint", "/bin/sh", "mariadb:11", "-c", "tail -f /dev/null")
        try:
            docker("cp", str(binary), f"{name}:/tmp/edge-test")
            result = docker(
                "exec", "-e", "TMPDIR=/data", "-e", "PHOENIX_EDGE_DISK_FULL_TEST=1",
                name, "/tmp/edge-test", "-test.v", "-test.timeout=180s",
                f"-test.run=^{DISK_TEST}$", check=False
            )
            output = result.stdout + result.stderr
            if result.returncode or f"--- PASS: {DISK_TEST}" not in output or "--- SKIP:" in output:
                raise RuntimeError(f"real tmpfs ENOSPC test did not pass: {output[-1800:]}")
            crash = docker(
                "exec", "-e", "TMPDIR=/data", name, "/tmp/edge-test", "-test.v",
                "-test.timeout=180s", f"-test.run=^{CRASH_TEST}$", check=False
            )
            crash_output = crash.stdout + crash.stderr
            named = [CRASH_TEST + "/inside-transaction", CRASH_TEST + "/after-commit", CRASH_TEST]
            if crash.returncode or "--- SKIP:" in crash_output or any(
                f"--- PASS: {test} " not in crash_output for test in named
            ):
                raise RuntimeError(f"real process-kill crash test did not pass: {crash_output[-1800:]}")
            print(json.dumps({"tests": [DISK_TEST, CRASH_TEST],
                              "engine": "modernc SQLite on real Linux tmpfs",
                              "tmpfs_bytes": 32 << 20, "no_network": True,
                              "named_passes": 4, "skips": 0, "result": "passed"}, indent=2))
        finally:
            docker("rm", "-f", name)


if __name__ == "__main__":
    main()
