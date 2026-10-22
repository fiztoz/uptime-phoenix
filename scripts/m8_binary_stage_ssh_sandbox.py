#!/usr/bin/env python3
"""Offline task-owned Linux/OpenSSH acceptance. Acquire a pinned public fixture separately with approval."""
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
import tarfile
import uuid

from m8_binary_stage_host_sandbox import GateError, command, need, save
from m8_binary_stage_ssh_acceptance import PACKAGE, TOP, audit_events
from m8_binary_stage_ssh_authorization_acceptance import TOP as AUTHORIZATION_TOP
from m8_binary_stage_ssh_authorization_acceptance import audit_events as audit_authorization_events

CAPABILITIES = ("SETUID", "SETGID", "SYS_CHROOT", "KILL")
FIXED_COMMAND = "/usr/local/bin/phoenix-probe-admin stage-binary-host"
PINNED_IMAGE = re.compile(r"(?!.*://)[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}\Z")


def fixture_scripts() -> dict[str, bytes]:
    """Controlled post-effect faults, never a production command or retry."""
    prefix = "#!/bin/sh\n[ \"$SSH_ORIGINAL_COMMAND\" = '" + FIXED_COMMAND + "' ] || exit 97\n"
    return {
        "post-stage-nonzero": (prefix + FIXED_COMMAND + "\ncode=$?\n[ \"$code\" -eq 0 ] || exit \"$code\"\nexit 7\n").encode(),
        "post-stage-lost-receipt": (prefix + FIXED_COMMAND + " >/dev/null\nexit $?\n").encode(),
    }


def acceptance_contract(authorization: bool) -> tuple:
    """Select one whole fixed test/parser/scope contract, without name overrides."""
    if authorization:
        return AUTHORIZATION_TOP, audit_authorization_events, "actual-linux-openssh-restricted-key"
    return TOP, audit_events, "actual-linux-openssh-binary-stage"


def create_arguments(label: str, image: str, runtime: bool, arch: str, authorization: bool = False) -> list[str]:
    """No runtime pull fallback, host namespace, bind, privilege or public network."""
    args = ["create", "--pull", "never", "--name", label + ("-run" if runtime else "-carrier"),
            "--label", "phoenix.m8.ssh=" + label, "--platform", "linux/" + arch,
            "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
            "--pids-limit", "128", "--memory", "1g", "--cpus", "2", "--user", "0:0",
            "--tmpfs", "/config:rw,nosuid,nodev,noexec,size=1m,mode=0700",
            "--entrypoint", "/fixtures/test"]
    if runtime:
        args += ["--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=512m,mode=1777",
                 "--tmpfs", "/opt:rw,nosuid,nodev,noexec,size=192m,mode=0755,uid=0,gid=0",
                 "--env", "TMPDIR=/tmp", "--env", "PHOENIX_M8_SSH_SANDBOX=1",
                 "--env", "PHOENIX_M8_SSH_SANDBOX_NONCE=" + label]
        for cap in CAPABILITIES:
            args += ["--cap-add", cap]
    if runtime and authorization:
        args += ["--env", "PHOENIX_M8_SSH_AUTHORIZATION_SANDBOX=1"]
    args += [image]
    if runtime:
        top, _, _ = acceptance_contract(authorization)
        args += ["-test.run=^" + top + "$", "-test.count=3", "-test.timeout=180s", "-test.v"]
    return args


def check_isolation(data: dict, label: str, runtime: bool) -> None:
    """Verify daemon facts, not just create argv."""
    hc = data["HostConfig"]
    caps = {x.removeprefix("CAP_") for x in (hc.get("CapAdd") or [])}
    need(hc["NetworkMode"] == "none" and not hc["Privileged"] and not hc.get("Binds") and
         hc.get("PidMode") != "host" and hc.get("IpcMode") != "host" and
         "ALL" in hc["CapDrop"] and caps == (set(CAPABILITIES) if runtime else set()) and
         hc["ReadonlyRootfs"] is runtime and
         any(x.startswith("no-new-privileges") for x in hc.get("SecurityOpt", [])) and
         not any(x.get("Type") in ("bind", "volume") for x in data.get("Mounts", [])) and
         data["Config"]["Labels"].get("phoenix.m8.ssh") == label and
         data["Config"]["User"] == "0:0" and data["Config"]["Entrypoint"] == ["/fixtures/test"] and
         hc["PidsLimit"] == 128 and hc["Memory"] == 1024 ** 3 and hc["NanoCpus"] == 2_000_000_000,
         "sandbox_isolation_invalid")
    expected_tmpfs = {"/config", "/tmp", "/opt"} if runtime else {"/config"}
    need(set(hc.get("Tmpfs", {})) == expected_tmpfs, "sandbox_tmpfs_invalid")


def add_bytes(tar: tarfile.TarFile, name: str, body: bytes, mode: int) -> None:
    item = tarfile.TarInfo(name)
    item.size, item.mode, item.uid, item.gid, item.mtime = len(body), mode, 0, 0, 0
    tar.addfile(item, io.BytesIO(body))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-local-docker", action="store_true")
    parser.add_argument("--authorization", action="store_true", help="run the complete restricted-key matrix instead of the unchanged default sender matrix")
    parser.add_argument("--docker-endpoint", required=True)
    parser.add_argument("--fixture-image", required=True, help="already cached repository@sha256; this gate never pulls")
    parser.add_argument("--go", required=True)
    parser.add_argument("--source-dir", default=str(Path(__file__).resolve().parents[1]))
    parser.add_argument("--module-cache", default=str(Path.home() / "go/pkg/mod"))
    parser.add_argument("--work-dir", required=True)
    args = parser.parse_args(argv)
    base = Path(args.work_dir).absolute()
    label = "phoenix-m8-ssh-" + uuid.uuid4().hex
    selected_top, selected_audit, selected_scope = acceptance_contract(args.authorization)
    result: dict = {"passed": False, "label": label, "pulled": False, "runtime_public_network": False,
                    "host_bind_mounts": False, "scoped_acceptance": selected_scope, "test_top": selected_top}
    docker: list[str] = []
    env: dict[str, str] = {}
    owned_base = False
    attempted = False
    sequence = 0
    containers: list[str] = []
    image_tags: list[str] = []
    previous_term = signal.getsignal(signal.SIGTERM)

    def interrupted(_number, _frame):
        raise GateError("interrupted")

    signal.signal(signal.SIGTERM, interrupted)

    def call(arguments: list[str], name: str, input_file: Path | None = None) -> str:
        nonlocal sequence
        sequence += 1
        code, output = command(docker + arguments, f"{sequence:02d}-{name}", base, env, base, 300, input_file)
        need(code == 0, "docker_command_failed")
        return output.strip()

    try:
        need(args.allow_local_docker, "docker_approval_required")
        need(args.docker_endpoint.startswith("unix:///") and not any(c in args.docker_endpoint for c in "\r\n?#"), "local_unix_endpoint_required")
        need(stat.S_ISSOCK(Path(args.docker_endpoint[7:]).stat().st_mode), "local_socket_required")
        need(PINNED_IMAGE.fullmatch(args.fixture_image) is not None, "pinned_cached_fixture_required")
        need(not base.exists(), "fresh_work_directory_required")
        base.mkdir(parents=True, mode=0o700)
        owned_base = True
        scratch = base / ".work"
        scratch.mkdir(mode=0o700)
        st = scratch.stat()
        save(base / "workspace-identity.json", {"dev": st.st_dev, "ino": st.st_ino, "uid": st.st_uid, "realpath": str(scratch.resolve())})
        config = scratch / "docker-config"
        config.mkdir(mode=0o700)
        (config / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        docker = ["docker", "--config", str(config), "--host", args.docker_endpoint]
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(scratch),
               "DOCKER_CONFIG": str(config), "DOCKER_HOST": args.docker_endpoint}
        before_containers = set(call(["ps", "-aq", "--no-trunc"], "baseline-containers").split())
        before_images = set(call(["image", "ls", "-q", "--no-trunc"], "baseline-images").split())
        info = json.loads(call(["info", "--format", '{"OSType":{{json .OSType}},"Architecture":{{json .Architecture}}}'], "daemon-info"))
        arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(info.get("Architecture"))
        need(info.get("OSType") == "linux" and arch is not None, "native_linux_daemon_required")
        image = json.loads(call(["image", "inspect", args.fixture_image], "cached-fixture-image"))[0]
        need(image["Os"] == "linux" and image["Architecture"] == arch and set(image["Config"].get("Volumes") or {}) <= {"/config"}, "fixture_image_contract_mismatch")
        result.update(runtime="linux/" + arch, fixture_image=args.fixture_image, fixture_image_id=image["Id"])
        go, source = Path(args.go).resolve(), Path(args.source_dir).resolve()
        need(go.is_file() and os.access(go, os.X_OK) and (source / "go.mod").is_file(), "installed_go_and_source_required")
        fixtures = scratch / "fixtures"
        fixtures.mkdir(mode=0o700)
        for name in ("cache", "tmp", "home"):
            (scratch / name).mkdir(mode=0o700)
        go_env = {"PATH": str(go.parent) + ":" + os.environ.get("PATH", ""), "HOME": str(scratch / "home"),
                  "GOMODCACHE": str(Path(args.module_cache).resolve()), "GOPATH": str(scratch / "gopath"),
                  "GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOWORK": "off",
                  "GOFLAGS": "", "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch, "GOMAXPROCS": "4",
                  "GOCACHE": str(scratch / "cache"), "TMPDIR": str(scratch / "tmp"), "GIT_CEILING_DIRECTORIES": str(source.parent)}
        for name, build in (("admin", ["build", "-buildvcs=false", "-o", str(fixtures / "admin"), "./cmd/phoenix-probe-admin"]),
                            ("test", ["test", "-c", "-buildvcs=false", "-o", str(fixtures / "test"), "./internal/adapters/provisioning"])):
            code, _ = command([str(go)] + build, "build-" + name, base, go_env, source)
            need(code == 0, "linux_fixture_build_failed")
        fixture_source = scratch / "probe-source"
        for relative, body in {
            "go.mod": "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
            "internal/version/version.go": 'package version\nvar Version="dev"\n',
            "cmd/probe/main.go": 'package main\nimport("os";"github.com/fiztoz/uptime-phoenix/internal/version")\nfunc main(){_ = os.WriteFile("/tmp/probe-executed", []byte(version.Version), 0600);os.Exit(99)}\n',
        }.items():
            target = fixture_source / relative
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            target.write_text(body, encoding="utf-8")
        for target_arch in ("amd64", "arm64"):
            code, _ = command([str(go), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=1.2.3",
                               "-o", str(fixtures / target_arch), "./cmd/probe"], "build-data-" + target_arch, base,
                              dict(go_env, GOARCH=target_arch), fixture_source)
            need(code == 0, "probe_data_fixture_build_failed")
        (fixtures / "ssh-sandbox-nonce").write_text(label, encoding="ascii")
        result["fixture_sha256"] = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in fixtures.iterdir()}
        archive = scratch / "fixture.tar"
        with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as tar:
            for name, mode in (("fixtures", 0o555), ("usr/local/bin", 0o755), ("var/empty", 0o755), ("run/sshd", 0o755), ("root", 0o700)):
                item = tarfile.TarInfo(name)
                item.type, item.uid, item.gid, item.mode, item.mtime = tarfile.DIRTYPE, 0, 0, mode, 0
                tar.addfile(item)
            for file in sorted(fixtures.iterdir()):
                add_bytes(tar, "fixtures/" + file.name, file.read_bytes(), 0o555 if file.name == "test" else 0o444)
            add_bytes(tar, "usr/local/bin/phoenix-probe-admin", (fixtures / "admin").read_bytes(), 0o555)
            for name, body in fixture_scripts().items():
                add_bytes(tar, "fixtures/" + name, body, 0o555)
            # Fixture-only account records: no real password/user credential.
            # NP is a non-lock marker, not a usable password; auth requires the
            # generated public key with password/PAM/interactive auth disabled.
            add_bytes(tar, "etc/passwd", b"root:x:0:0:root:/root:/bin/sh\nsshd:x:22:22:sshd:/var/empty:/sbin/nologin\n", 0o644)
            add_bytes(tar, "etc/group", b"root:x:0:\nsshd:x:22:\n", 0o644)
            add_bytes(tar, "etc/shadow", b"root:NP:0:0:99999:7:::\nsshd:*:0:0:99999:7:::\n", 0o600)
        attempted = True
        carrier = call(create_arguments(label, args.fixture_image, False, arch, args.authorization), "create-carrier")
        need(re.fullmatch(r"[0-9a-f]{64}", carrier) is not None, "container_identity_invalid")
        containers.append(carrier)
        check_isolation(json.loads(call(["inspect", carrier], "inspect-carrier"))[0], label, False)
        call(["cp", "-a", "-", carrier + ":/"], "copy-fixtures", archive)
        tag = label + ":fixture"
        image_tags.append(tag)
        derived = call(["commit", "--change", "LABEL phoenix.m8.ssh=" + label, "--change", "ENTRYPOINT []", "--change", "CMD []", "--change", "USER 0:0", carrier, tag], "commit-fixture")
        need(re.fullmatch(r"sha256:[0-9a-f]{64}", derived) is not None, "image_identity_invalid")
        result["owned_fixture_image"] = derived
        container = call(create_arguments(label, derived, True, arch, args.authorization), "create-runtime")
        need(re.fullmatch(r"[0-9a-f]{64}", container) is not None, "container_identity_invalid")
        containers.append(container)
        check_isolation(json.loads(call(["inspect", container], "inspect-runtime"))[0], label, True)
        code, _ = command(docker + ["start", "--attach", container], "run-openssh", base, env, base, timeout=240)
        final = json.loads(call(["inspect", container], "runtime-exit"))[0]
        need(not final["State"]["Running"] and not final["State"]["OOMKilled"] and code == final["State"]["ExitCode"], "container_exit_unproved")
        host_go_env = {k: v for k, v in go_env.items() if k not in ("GOOS", "GOARCH")}
        converted, text = command([str(go), "tool", "test2json", "-t", "-p", PACKAGE], "go-events", base, host_go_env, source, input_file=base / "run-openssh/stdout")
        need(converted == 0, "test2json_failed")
        result["acceptance"] = selected_audit(text, 3, code)
        need(result["acceptance"]["passed"], "openssh_acceptance_refused")
        result["baseline_containers_preserved"] = before_containers <= set(call(["ps", "-aq", "--no-trunc"], "baseline-containers-after").split())
        result["baseline_images_preserved"] = before_images <= set(call(["image", "ls", "-q", "--no-trunc"], "baseline-images-after").split())
        need(result["baseline_containers_preserved"] and result["baseline_images_preserved"], "baseline_resource_drift")
        result["passed"] = True
    except (GateError, OSError, ValueError, KeyError, TypeError, KeyboardInterrupt) as exc:
        result["error"] = str(exc) if isinstance(exc, GateError) else "interrupted" if isinstance(exc, KeyboardInterrupt) else "fixture_setup_or_protocol_failure"
    finally:
        cleanup_errors: list[str] = []
        if docker and attempted:
            try:
                found = call(["ps", "-aq", "--no-trunc", "--filter", "label=phoenix.m8.ssh=" + label], "discover-owned-containers").split()
                containers = sorted(set(containers + found))
                tags = call(["image", "ls", "--filter", "label=phoenix.m8.ssh=" + label, "--format", "{{.Repository}}:{{.Tag}}"], "discover-owned-images").split()
                need(all(tag == label + ":fixture" for tag in tags), "cleanup_tag_mismatch")
                image_tags = sorted(set(image_tags + tags))
                for identity in containers:
                    data = json.loads(call(["inspect", identity], "cleanup-inspect-container"))[0]
                    need(data["Config"]["Labels"].get("phoenix.m8.ssh") == label, "cleanup_owner_mismatch")
                    if data["State"]["Running"]:
                        call(["kill", identity], "cleanup-kill-container")
                    call(["rm", identity], "cleanup-remove-container")
                for identity in image_tags:
                    data = json.loads(call(["image", "inspect", identity], "cleanup-inspect-image"))[0]
                    need(data["Config"]["Labels"].get("phoenix.m8.ssh") == label, "cleanup_owner_mismatch")
                    call(["image", "rm", identity], "cleanup-remove-image")
                need(not call(["ps", "-aq", "--filter", "label=phoenix.m8.ssh=" + label], "remaining-owned-containers"), "owned_containers_remain")
                need(not call(["image", "ls", "-q", "--filter", "label=phoenix.m8.ssh=" + label], "remaining-owned-images"), "owned_images_remain")
            except (GateError, OSError, ValueError, KeyError, TypeError):
                cleanup_errors.append("owned_resource_cleanup_unproved")
        result["cleanup_errors"] = cleanup_errors
        result["owned_containers"] = containers
        result["owned_image_tags"] = image_tags
        if cleanup_errors:
            result["passed"] = False
        if owned_base:
            save(base / "result.json", result)
        signal.signal(signal.SIGTERM, previous_term)
        print(json.dumps({"passed": result["passed"], "error": result.get("error"), "cleanup_errors": len(cleanup_errors)}, sort_keys=True))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
