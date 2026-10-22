#!/usr/bin/env python3
"""Run the explicitly approved, credential-free local M8 Linux sandbox gate.

No pulls/build daemon/network, host root bind, Docker socket mount, privileged
container, application deployment or probe artifact execution. Fixtures are
copied into task-owned scratch images/read-only volumes. Two explicitly approved
root-inode cases use private writable layers and one fixture-only CHOWN capability;
the test clears capabilities and reexecutes before exercising the helper.
"""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tarfile
import time
import uuid

from m8_binary_stage_host_acceptance import audit_events


SCENARIOS = ("positive", "unsafe_owner", "missing_opt", "symlink_opt", "regular_opt", "nonroot", "unsafe_root_owner", "unsafe_root_mode")


class GateError(Exception):
    """A fixed, secrecy-safe gate failure."""


def complete_matrix_verdict(results: dict) -> None:
    scenarios = results["scenarios"]
    subset_passed = bool(scenarios) and all(verdict.get("passed") is True for verdict in scenarios.values())
    results["diagnostic_passed"] = subset_passed
    results["passed"] = subset_passed and results.get("full_matrix") is True and set(scenarios) == set(SCENARIOS)
    if not results["passed"]:
        results["error"] = "partial_matrix_not_acceptance" if subset_passed else "matrix_incomplete"


def need(value: bool, reason: str) -> None:
    if not value:
        raise GateError(reason)


def save(path: Path, value: dict) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    path.chmod(0o600)


def command(args: list[str], label: str, base: Path, env: dict[str, str], cwd: Path,
            timeout: int = 600, input_file: Path | None = None) -> tuple[int, str]:
    folder = base / label
    folder.mkdir(mode=0o700)
    start = time.monotonic()
    timed_out = False
    with (folder / "stdout").open("xb") as out, (folder / "stderr").open("xb") as err:
        source = input_file.open("rb") if input_file else subprocess.DEVNULL
        proc = None
        try:
            proc = subprocess.Popen(args, cwd=cwd, env=env, stdin=source,
                                    stdout=out, stderr=err, start_new_session=True)
            try:
                code = proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                timed_out = True
                os.killpg(proc.pid, signal.SIGTERM)
                try:
                    code = proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    code = proc.wait()
        except BaseException:
            if proc is not None and proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait()
            raise
        finally:
            if input_file:
                source.close()
    save(folder / "result.json", {"exit": code, "timed_out": timed_out,
         "seconds": round(time.monotonic() - start, 3), "command": args})
    need(not timed_out, "command_timeout")
    need((folder / "stdout").stat().st_size <= 32 * 1024 * 1024 and
         (folder / "stderr").stat().st_size <= 32 * 1024 * 1024, "log_bound")
    return code, (folder / "stdout").read_text(encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-local-docker", action="store_true")
    parser.add_argument("--docker-endpoint", required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--work-dir", required=True)
    parser.add_argument("--source-dir", default=str(Path(__file__).resolve().parents[1]))
    scenario_names = list(SCENARIOS)
    parser.add_argument("--scenario", choices=["all"] + scenario_names, default="all",
                        help="single-scenario diagnostics are not whole-matrix acceptance")
    args = parser.parse_args(argv)
    base = Path(args.work_dir).absolute()
    images: list[str] = []
    image_tags: list[str] = []
    containers: list[str] = []
    volumes: list[str] = []
    owned_base = False
    resources_attempted = False
    label = "phoenix-m8-host-" + uuid.uuid4().hex
    results: dict = {"passed": False, "label": label, "scenarios": {},
                     "network": False, "pulled": False, "host_bind_mounts": False,
                     "full_matrix": args.scenario == "all"}
    docker: list[str] = []
    env: dict[str, str] = {}
    sequence = 0
    previous_term = signal.getsignal(signal.SIGTERM)

    def interrupted(_number, _frame):
        raise GateError("interrupted")

    signal.signal(signal.SIGTERM, interrupted)

    def call(arguments: list[str], name: str, timeout: int = 600,
             input_file: Path | None = None) -> str:
        nonlocal sequence
        sequence += 1
        code, output = command(docker + arguments, f"{sequence:02d}-{name}", base,
                               env, base, timeout, input_file)
        need(code == 0, "docker_command_failed")
        return output.strip()

    try:
        need(args.allow_local_docker, "docker_approval_required")
        need(args.docker_endpoint.startswith("unix:///") and
             not any(c in args.docker_endpoint for c in "\n\r?#"), "local_unix_endpoint_required")
        socket_path = Path(args.docker_endpoint[7:])
        need(stat.S_ISSOCK(socket_path.stat().st_mode), "local_socket_required")
        need(not base.exists(), "fresh_work_directory_required")
        base.mkdir(parents=True, mode=0o700)
        owned_base = True
        scratch = base / ".work"
        scratch.mkdir(mode=0o700)
        st = scratch.stat()
        save(base / "workspace-identity.json", {"dev": st.st_dev, "ino": st.st_ino,
             "uid": st.st_uid, "realpath": str(scratch.resolve())})
        config = scratch / "docker-config"
        config.mkdir(mode=0o700)
        (config / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        docker = ["docker", "--config", str(config), "--host", args.docker_endpoint]
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(scratch),
               "DOCKER_CONFIG": str(config), "DOCKER_HOST": args.docker_endpoint}
        baseline_containers = set(call(["ps", "-aq", "--no-trunc"], "baseline-containers").split())
        baseline_images = set(call(["image", "ls", "-q", "--no-trunc"], "baseline-images").split())
        info = json.loads(call(["info", "--format", '{"OSType":{{json .OSType}},"Architecture":{{json .Architecture}}}'], "daemon-info"))
        arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(info.get("Architecture"))
        need(info.get("OSType") == "linux" and arch is not None, "native_linux_daemon_required")
        results["runtime"] = "linux/" + arch
        go = Path(args.go).resolve()
        need(go.is_file() and os.access(go, os.X_OK), "installed_go_required")
        source_root = Path(args.source_dir).resolve()
        need((source_root / "go.mod").is_file(), "source_root_required")
        cache, tmp, fixtures = scratch / "cache", scratch / "tmp", scratch / "fixtures"
        for directory in (cache, tmp, fixtures):
            directory.mkdir(mode=0o700)
        go_env = {"PATH": str(go.parent) + ":" + os.environ.get("PATH", ""),
                  "HOME": os.environ.get("HOME", ""), "GOENV": "off", "GOTOOLCHAIN": "local",
                  "GOPROXY": "off", "GOSUMDB": "off", "GOWORK": "off", "GOFLAGS": "",
                  "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch,
                  "GOCACHE": str(cache), "TMPDIR": str(tmp),
                  "GIT_CEILING_DIRECTORIES": str(source_root.parent)}
        for name, arguments in [("admin", ["build", "-buildvcs=false", "-o", str(fixtures / "admin"), "./cmd/phoenix-probe-admin"]),
                                ("test", ["test", "-c", "-buildvcs=false", "-o", str(fixtures / "test"), "./internal/bootstrap"])]:
            code, _ = command([str(go)] + arguments, "build-" + name, base, go_env, source_root)
            need(code == 0, "linux_fixture_build_failed")
        probe_source = scratch / "probe-source"
        for relative, body in {
            "go.mod": "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
            "internal/version/version.go": 'package version\nvar Version="dev"\n',
            "cmd/probe/main.go": 'package main\nimport("github.com/fiztoz/uptime-phoenix/internal/version")\nfunc main(){println(version.Version)}\n',
        }.items():
            target = probe_source / relative
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            target.write_text(body, encoding="utf-8")
        for target_arch in ("amd64", "arm64"):
            fixture_env = dict(go_env, GOARCH=target_arch)
            code, _ = command([str(go), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=1.2.3",
                               "-o", str(fixtures / target_arch), "./cmd/probe"],
                              "build-probe-" + target_arch, base, fixture_env, probe_source)
            need(code == 0, "linux_probe_fixture_build_failed")
        (fixtures / "sandbox-nonce").write_text(label, encoding="ascii")
        results["fixture_sha256"] = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in fixtures.iterdir()}
        variants = {"base": (0, 0o755, None), "symlink": (0, 0o755, "symlink"),
                    "regular": (0, 0o755, "regular")}
        selected = scenario_names if args.scenario == "all" else [args.scenario]
        variant_for = {"symlink_opt": "symlink", "regular_opt": "regular"}
        needed_variants = {variant_for.get(scenario, "base") for scenario in selected}
        image_ids: dict[str, str] = {}
        for variant, (uid, mode, opt_kind) in variants.items():
            if variant not in needed_variants:
                continue
            archive = scratch / (variant + ".tar")
            with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as tar:
                for name, owner, perm in [(".", uid, mode), ("fixtures", 0, 0o555)]:
                    item = tarfile.TarInfo(name)
                    item.type, item.uid, item.gid, item.mode, item.mtime = tarfile.DIRTYPE, owner, 0, perm, 0
                    tar.addfile(item)
                for file in sorted(fixtures.iterdir()):
                    item = tarfile.TarInfo("fixtures/" + file.name)
                    item.size, item.mode, item.uid, item.gid, item.mtime = file.stat().st_size, 0o555 if file.name in ("admin", "test") else 0o444, 0, 0, 0
                    with file.open("rb") as stream:
                        tar.addfile(item, stream)
                if opt_kind:
                    item = tarfile.TarInfo("opt")
                    item.uid, item.gid, item.mode, item.mtime = 0, 0, 0o755, 0
                    if opt_kind == "symlink":
                        item.type, item.linkname = tarfile.SYMTYPE, "real-opt"
                        tar.addfile(item)
                        target = tarfile.TarInfo("real-opt")
                        target.type, target.uid, target.gid, target.mode, target.mtime = tarfile.DIRTYPE, 0, 0, 0o755, 0
                        tar.addfile(target)
                    else:
                        body = b"not a directory\n"
                        item.size, item.mode = len(body), 0o444
                        tar.addfile(item, io.BytesIO(body))
            resources_attempted = True
            image = call(["import", "--platform", "linux/" + arch, "--change", "LABEL phoenix.m8.host=" + label,
                          "-", label + ":" + variant], "import-" + variant, input_file=archive)
            need(re.fullmatch(r"sha256:[0-9a-f]{64}", image) is not None, "image_identity_invalid")
            images.append(image)
            image_tags.append(label + ":" + variant)
            image_ids[variant] = image
        for scenario in selected:
            variant = {"symlink_opt": "symlink", "regular_opt": "regular"}.get(scenario, "base")
            root_fixture = scenario in ("unsafe_root_owner", "unsafe_root_mode")
            argv_create = ["create", "--pull", "never", "--name", label + "-" + scenario, "--label", "phoenix.m8.host=" + label,
                           "--platform", "linux/" + arch, "--network", "none", "--cap-drop", "ALL",
                           "--security-opt", "no-new-privileges", "--pids-limit", "96", "--memory", "768m", "--cpus", "1",
                           "--user", "10001:10001" if scenario == "nonroot" else "0:0",
                           "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=512m,mode=1777",
                           "--env", "TMPDIR=/tmp", "--env", "PHOENIX_M8_HOST_SANDBOX=1",
                           "--env", "PHOENIX_M8_HOST_SANDBOX_NONCE=" + label,
                           "--env", "PHOENIX_M8_HOST_SANDBOX_SCENARIO=" + scenario]
            if root_fixture:
                if not volumes:
                    volume = call(["volume", "create", "--label", "phoenix.m8.host=" + label, label + "-fixtures"], "create-fixture-volume")
                    need(volume == label + "-fixtures", "volume_identity_invalid")
                    volumes.append(volume)
                argv_create += ["--env", "PHOENIX_M8_HOST_SANDBOX_SETUP=1", "--mount", "type=volume,source=" + volumes[0] + ",target=/fixtures,readonly"]
                if scenario == "unsafe_root_owner":
                    argv_create += ["--cap-add", "CHOWN"]
            else:
                argv_create += ["--read-only"]
            if scenario not in ("missing_opt", "symlink_opt", "regular_opt"):
                argv_create += ["--tmpfs", "/opt:rw,nosuid,nodev,noexec,size=192m,mode=0755,uid=" + ("10001" if scenario == "unsafe_owner" else "0")]
            argv_create += [image_ids[variant], "/fixtures/test", "-test.run=^TestProbeBinaryStageHostSandbox$", "-test.count=3", "-test.timeout=300s", "-test.v"]
            container = call(argv_create, "create-" + scenario)
            need(re.fullmatch(r"[0-9a-f]{64}", container) is not None, "container_identity_invalid")
            containers.append(container)
            inspected = json.loads(call(["inspect", container], "inspect-" + scenario))[0]
            hc = inspected["HostConfig"]
            mounts = inspected.get("Mounts", [])
            expected_cap = ["CHOWN"] if scenario == "unsafe_root_owner" else []
            need(hc["ReadonlyRootfs"] == (not root_fixture) and hc["NetworkMode"] == "none" and not hc["Privileged"] and
                 "ALL" in hc["CapDrop"] and [cap.removeprefix("CAP_") for cap in (hc.get("CapAdd") or [])] == expected_cap and not hc.get("Binds") and hc.get("PidMode") != "host" and
                 all(mount.get("Type") != "bind" for mount in mounts) and
                 (not root_fixture or any(mount.get("Type") == "volume" and mount.get("Name") == volumes[0] and mount.get("Destination") == "/fixtures" and not mount.get("RW") for mount in mounts)) and
                 inspected["Config"]["Labels"].get("phoenix.m8.host") == label, "sandbox_isolation_invalid")
            sequence += 1
            run_label = f"{sequence:02d}-run-{scenario}"
            run_exit, _ = command(docker + ["start", "--attach", container], run_label, base, env, base, timeout=360)
            final = json.loads(call(["inspect", container], "exit-" + scenario))[0]
            exit_code = final["State"]["ExitCode"]
            need(not final["State"]["Running"] and not final["State"]["OOMKilled"] and run_exit == exit_code, "container_exit_unproved")
            json_label = "json-" + scenario
            host_go_env = dict(go_env)
            host_go_env.pop("GOOS", None)
            host_go_env.pop("GOARCH", None)
            code, text = command([str(go), "tool", "test2json", "-t", "-p", "github.com/fiztoz/uptime-phoenix/internal/bootstrap"],
                                 json_label, base, host_go_env, source_root, input_file=base / run_label / "stdout")
            need(code == 0, "test2json_failed")
            verdict = audit_events(text, scenario, 3, exit_code)
            results["scenarios"][scenario] = verdict
            save(base / "progress.json", results)
            need(verdict["passed"], "linux_acceptance_refused")
        complete_matrix_verdict(results)
        results["baseline_containers_preserved"] = baseline_containers.issubset(set(call(["ps", "-aq", "--no-trunc"], "baseline-containers-after").split()))
        results["baseline_images_preserved"] = baseline_images.issubset(set(call(["image", "ls", "-q", "--no-trunc"], "baseline-images-after").split()))
        need(results["baseline_containers_preserved"] and results["baseline_images_preserved"], "baseline_resource_drift")
    except (GateError, OSError, ValueError, KeyError, TypeError, KeyboardInterrupt) as exc:
        results["passed"] = False
        results["error"] = str(exc) if isinstance(exc, GateError) else "interrupted" if isinstance(exc, KeyboardInterrupt) else "sandbox_setup_or_protocol_failure"
    finally:
        cleanup_errors: list[str] = []
        if docker and resources_attempted:
            try:
                # Recover creations even if their client acknowledgement was lost.
                observed_containers = call(["ps", "-aq", "--no-trunc", "--filter", "label=phoenix.m8.host=" + label], "cleanup-discover-containers").split()
                containers = sorted(set(containers + observed_containers))
                observed_tags = call(["image", "ls", "--filter", "label=phoenix.m8.host=" + label, "--format", "{{.Repository}}:{{.Tag}}"], "cleanup-discover-images").split()
                need(all(tag.startswith(label + ":") for tag in observed_tags), "cleanup_tag_mismatch")
                image_tags = sorted(set(image_tags + observed_tags))
            except (GateError, OSError, ValueError, KeyError, TypeError):
                cleanup_errors.append("owned_resource_discovery_unproved")
            try:
                found_volumes = call(["volume", "ls", "-q", "--filter", "label=phoenix.m8.host=" + label], "cleanup-discover-volumes").split()
                need(all(volume == label + "-fixtures" for volume in found_volumes), "cleanup_volume_mismatch")
                volumes = sorted(set(volumes + found_volumes))
            except (GateError, OSError, ValueError, KeyError, TypeError):
                cleanup_errors.append("owned_volume_discovery_unproved")
            for kind, ids in [("container", containers), ("image", image_tags), ("volume", volumes)]:
                for identity in ids:
                    try:
                        inspect_args = ["inspect", identity] if kind == "container" else [kind, "inspect", identity]
                        data = json.loads(call(inspect_args, "cleanup-inspect-" + kind))[0]
                        labels = data.get("Labels", {}) if kind == "volume" else data["Config"]["Labels"]
                        need(labels.get("phoenix.m8.host") == label, "cleanup_owner_mismatch")
                        if kind == "container":
                            if data["State"]["Running"]:
                                call(["kill", identity], "cleanup-kill")
                            call(["rm", identity], "cleanup-container")
                        else:
                            call([kind, "rm", identity], "cleanup-" + kind)
                    except (GateError, OSError, ValueError, KeyError, TypeError):
                        cleanup_errors.append("owned_resource_cleanup_unproved")
        results["cleanup_errors"] = cleanup_errors
        results["owned_containers"] = containers
        results["owned_images"] = images
        results["owned_volumes"] = volumes
        if cleanup_errors:
            results["passed"] = False
        if owned_base:
            save(base / "result.json", results)
        signal.signal(signal.SIGTERM, previous_term)
        print(json.dumps({"passed": results["passed"], "error": results.get("error"),
                          "scenarios": len(results["scenarios"]), "cleanup_errors": len(cleanup_errors)}, sort_keys=True))
    return 0 if results["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
