#!/usr/bin/env python3
"""Build and verify a local probe OCI export; never publish, load or run it.

Requires explicit public-pull approval and a local Docker Unix socket. Uses only
stdlib plus already-installed Docker/Buildx and Go. Reports fixed errors; private
command logs are retained in a newly created work directory. This establishes
local build correspondence, not publisher, rootfs or runtime-version authority.
"""
from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import selectors
import signal
import stat
import subprocess
import tarfile
import time


class GateError(Exception):
    """A fixed, non-secret acceptance refusal."""


SHA = re.compile(r"sha256:([0-9a-f]{64})\Z")
INDEX_MEDIA = "application/vnd.oci.image.index.v1+json"
MANIFEST_MEDIA = "application/vnd.oci.image.manifest.v1+json"
META_LIMIT = 4 * 1024 * 1024
BLOB_LIMIT = 128 * 1024 * 1024
ARCHIVE_LIMIT = 1024 * 1024 * 1024


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def strict_json(data: bytes) -> dict:
    """Decode bounded UTF-8 object JSON without duplicate keys or NaN."""
    if len(data) > META_LIMIT:
        raise GateError("JSON limit exceeded")

    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise GateError("duplicate JSON key")
            result[key] = value
        return result

    def constant(_):
        raise GateError("nonfinite JSON number")

    try:
        result = json.loads(data.decode("utf-8"), object_pairs_hook=pairs,
                            parse_constant=constant)
    except (UnicodeError, ValueError, RecursionError) as exc:
        raise GateError("invalid JSON") from exc
    if not isinstance(result, dict):
        raise GateError("JSON object required")
    return result


def read_json(path: Path) -> dict:
    with path.open("rb") as stream:
        return strict_json(stream.read(META_LIMIT + 1))


def save(path: Path, value: dict) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")
    path.chmod(0o600)


def safe_name(name: str, directory: bool = False) -> str:
    """Allow canonical relative archive names, never links or traversal."""
    if directory:
        name = name.rstrip("/")
    parts = PurePosixPath(name).parts
    if (not name or len(name) > 512 or "\\" in name or "\0" in name
            or name.startswith("/") or any(p in (".", "..") for p in name.split("/"))
            or str(PurePosixPath(name)) != name or not parts):
        raise GateError("unsafe archive name")
    return name


def copy_exact(source, target: Path, size: int) -> str:
    digest = hashlib.sha256()
    remaining = size
    with target.open("xb") as out:
        while remaining:
            block = source.read(min(65536, remaining))
            if not block:
                raise GateError("truncated archive member")
            out.write(block)
            digest.update(block)
            remaining -= len(block)
    target.chmod(0o600)
    return digest.hexdigest()


def snapshot_source(archive: Path, destination: Path) -> None:
    """Copy a bounded tracked Git archive, not the mutable working directory."""
    if archive.stat().st_size > 128 * 1024 * 1024:
        raise GateError("source archive limit exceeded")
    destination.mkdir(mode=0o700)
    seen, total = set(), 0
    with tarfile.open(archive, "r:") as source:
        for member in source:
            name = safe_name(member.name, member.isdir())
            if name in seen or len(seen) >= 10000:
                raise GateError("source archive duplicate/count limit")
            seen.add(name)
            target = destination / name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True, mode=0o700)
                continue
            if (not member.isfile() or member.sparse is not None
                    or member.size < 0 or member.size > 16 * 1024 * 1024):
                raise GateError("unsafe source member")
            total += member.size
            if total > 128 * 1024 * 1024:
                raise GateError("source size limit exceeded")
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            stream = source.extractfile(member)
            if stream is None:
                raise GateError("source member unavailable")
            with stream:
                copy_exact(stream, target, member.size)
            if member.mode & 0o111:
                target.chmod(0o700)


def unpack_oci(archive: Path, destination: Path, *, blob_limit=BLOB_LIMIT,
               total_limit=ARCHIVE_LIMIT, entry_limit=1024) -> tuple[dict, dict]:
    """Read only layout metadata and opaque hash-named blobs; no rootfs extract."""
    if archive.stat().st_size > total_limit + 2 * 1024 * 1024:
        raise GateError("OCI archive size limit exceeded")
    destination.mkdir(mode=0o700)
    seen, blobs, metadata, total = set(), {}, {}, 0
    with tarfile.open(archive, "r:") as source:
        for member in source:
            name = safe_name(member.name, member.isdir())
            if name in seen or len(seen) >= entry_limit:
                raise GateError("OCI duplicate/count limit")
            seen.add(name)
            if member.isdir():
                if name not in ("blobs", "blobs/sha256"):
                    raise GateError("unexpected OCI directory")
                continue
            if not member.isfile() or member.sparse is not None:
                raise GateError("unsafe OCI member")
            match = re.fullmatch(r"blobs/sha256/([0-9a-f]{64})", name)
            if name not in ("oci-layout", "index.json") and not match:
                raise GateError("unexpected OCI member")
            limit = blob_limit if match else META_LIMIT
            if member.size <= 0 or member.size > limit:
                raise GateError("OCI member size limit exceeded")
            total += member.size
            if total > total_limit:
                raise GateError("OCI total size limit exceeded")
            stream = source.extractfile(member)
            if stream is None:
                raise GateError("OCI member unavailable")
            target = destination / (match[1] if match else name)
            with stream:
                actual = copy_exact(stream, target, member.size)
            if match:
                if actual != match[1]:
                    raise GateError("OCI blob digest mismatch")
                blobs[actual] = target
            else:
                metadata[name] = target
    if set(metadata) != {"oci-layout", "index.json"}:
        raise GateError("OCI layout metadata missing")
    if read_json(metadata["oci-layout"]) != {"imageLayoutVersion": "1.0.0"}:
        raise GateError("unsupported OCI layout")
    return blobs, metadata


def pinned_dockerfile(original: str, pins: dict[str, str]) -> str:
    """Change only the two actual Dockerfile base references, never build logic."""
    pattern = re.compile(r"^(FROM (?:--platform=\$BUILDPLATFORM )?)(\S+)( AS go-builder)?$", re.MULTILINE)
    refs = [m[2] for m in pattern.finditer(original)]
    if (len(refs) != 2 or not re.fullmatch(r"golang:[0-9]+\.[0-9]+\.[0-9]+-alpine", refs[0])
            or refs[1] != "gcr.io/distroless/static-debian12:nonroot"
            or set(pins) != set(refs) or any(not SHA.fullmatch(v) for v in pins.values())):
        raise GateError("unexpected Dockerfile/base pin contract")
    return pattern.sub(lambda m: m[1] + m[2] + "@" + pins[m[2]] + (m[3] or ""), original)


def choose_dockerfile(tracked: str, working: str, allow_overlay: bool) -> tuple[str, bool]:
    """Never silently build a stale tracked Dockerfile while verifying a local fix."""
    if tracked == working:
        return tracked, False
    if not allow_overlay:
        raise GateError("working Dockerfile differs; explicit overlay approval required")
    return working, True


def copied_probe(layer: Path, media: str, target: Path, arch: str) -> dict:
    """Read the final COPY's one regular ELF to a generated path, never tar paths."""
    if media == "application/vnd.oci.image.layer.v1.tar+gzip":
        opener = gzip.open
    elif media == "application/vnd.oci.image.layer.v1.tar":
        opener = open
    else:
        raise GateError("unsupported final COPY encoding")
    found, total = 0, 0
    with opener(layer, "rb") as decoded, tarfile.open(fileobj=decoded, mode="r|") as archive:
        for number, member in enumerate(archive):
            if number >= 32 or member.size < 0 or member.size > BLOB_LIMIT:
                raise GateError("final COPY size/count limit")
            total += member.size
            if total > 512 * 1024 * 1024:
                raise GateError("final COPY total limit")
            if member.isdir() and member.name in ("/", ".", "./") and member.size == 0:
                continue
            if (not member.isfile() or member.sparse is not None
                    or member.name not in ("uptime-phoenix-probe", "/uptime-phoenix-probe")
                    or found or member.size < 64):
                raise GateError("unexpected final COPY member")
            stream = archive.extractfile(member)
            if stream is None:
                raise GateError("copied executable unavailable")
            with stream:
                digest = copy_exact(stream, target, member.size)
            found += 1
    if found != 1:
        raise GateError("exactly one copied executable required")
    with target.open("rb") as source:
        header = source.read(64)
    if (header[:4] != b"\x7fELF" or header[4:6] != b"\x02\x01"
            or arch not in ("amd64", "arm64")
            or int.from_bytes(header[18:20], "little") != {"amd64": 62, "arm64": 183}[arch]):
        raise GateError("copied probe ELF architecture mismatch")
    return {"sha256": digest, "size": target.stat().st_size,
            "elf_machine": int.from_bytes(header[18:20], "little")}


def build_args(context: Path, archive: Path, metadata: Path, version: str,
               platform: str = "linux/amd64,linux/arm64") -> list[str]:
    return ["docker", "buildx", "build", "--platform", platform,
            "--pull", "--no-cache", "--provenance=mode=max", "--progress=plain",
            "--build-arg", "VERSION=" + version, "--metadata-file", str(metadata),
            "--output", "type=oci,oci-mediatypes=true,compression=gzip,force-compression=true,dest=" + str(archive),
            "-f", str(context / "Dockerfile.probe"), str(context)]


def isolated_env(scratch: Path, config: Path, endpoint: str) -> dict:
    """Do not inherit DSNs, application secrets, Docker auth, proxies or Go config."""
    env = {k: os.environ[k] for k in ("PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE") if k in os.environ}
    env.update({"LANG": "C", "DOCKER_CONFIG": str(config), "DOCKER_HOST": endpoint,
                "GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
                "GOWORK": "off", "TMPDIR": str(scratch)})
    return env


class Runner:
    """Execute joined local clients with private bounded logs and fixed errors."""

    def __init__(self, work: Path, env: dict, timeout: int):
        self.work, self.env, self.timeout = work, env, timeout
        self.commands = []

    def call(self, label: str, args: list[str], cwd: Path, *, expect=0,
             limit=16 * 1024 * 1024) -> bytes:
        self.commands.append({"label": label, "argv": args, "cwd": str(cwd)})
        selector, outputs = selectors.DefaultSelector(), [bytearray(), bytearray()]
        process = subprocess.Popen(args, cwd=cwd, env=self.env, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
        assert process.stdout is not None and process.stderr is not None
        selector.register(process.stdout, selectors.EVENT_READ, 0)
        selector.register(process.stderr, selectors.EVENT_READ, 1)
        deadline = time.monotonic() + self.timeout
        try:
            while selector.get_map():
                if time.monotonic() >= deadline:
                    raise GateError(label + ": local client timeout")
                for key, _ in selector.select(timeout=0.2):
                    block = os.read(key.fileobj.fileno(), 65536)
                    if not block:
                        selector.unregister(key.fileobj)
                    else:
                        outputs[key.data].extend(block)
                        if len(outputs[key.data]) > limit:
                            raise GateError(label + ": command log limit exceeded")
            remaining = max(0.01, deadline - time.monotonic())
            code = process.wait(timeout=remaining)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            selector.close()
            process.stdout.close()
            process.stderr.close()
            for suffix, data in zip(("stdout", "stderr"), outputs):
                with (self.work / (label + "." + suffix)).open("xb") as output:
                    output.write(data)
            with (self.work / (label + ".exit")).open("x", encoding="ascii") as output:
                output.write(str(process.returncode))
        if (expect == 0 and code != 0) or (expect != 0 and code != expect):
            raise GateError(label + ": command did not satisfy expected exit")
        return bytes(outputs[0])


def blob_for(blobs: dict, descriptor: dict) -> Path:
    digest = descriptor.get("digest")
    match = SHA.fullmatch(digest) if isinstance(digest, str) else None
    size = descriptor.get("size")
    if (not match or type(size) is not int or size <= 0 or size > BLOB_LIMIT
            or match[1] not in blobs or blobs[match[1]].stat().st_size != size):
        raise GateError("missing/invalid OCI descriptor")
    return blobs[match[1]]


def verify_export(work: Path, archive: Path, metadata: Path, admin: Path,
                  version: str, runner: Runner, *, architectures=("amd64", "arm64")) -> dict:
    built = read_json(metadata)
    root_digest = built.get("containerimage.digest")
    match = SHA.fullmatch(root_digest) if isinstance(root_digest, str) else None
    if not match:
        raise GateError("local build index digest missing")
    blobs, layout = unpack_oci(archive, work / ".blobs")
    index_path = blobs.get(match[1], layout["index.json"])
    if sha(index_path.read_bytes()) != match[1]:
        raise GateError("export disagrees with local build index")
    index = read_json(index_path)
    if index.get("mediaType") != INDEX_MEDIA or index.get("schemaVersion") != 2:
        raise GateError("actual multiarch OCI index required")
    manifests = index.get("manifests")
    if not isinstance(manifests, list) or not 1 <= len(manifests) <= 256:
        raise GateError("invalid OCI platform list")
    results = {}
    for arch in architectures:
        if arch not in ("amd64", "arm64"):
            raise GateError("unsupported requested executable platform")
        selected = [d for d in manifests if isinstance(d, dict)
                    and isinstance(d.get("platform"), dict)
                    and d["platform"].get("os") == "linux"
                    and d["platform"].get("architecture") == arch]
        if len(selected) != 1 or selected[0].get("mediaType") != MANIFEST_MEDIA:
            raise GateError("exact executable platform missing/ambiguous")
        manifest_path = blob_for(blobs, selected[0])
        manifest = read_json(manifest_path)
        config_path = blob_for(blobs, manifest["config"])
        layers = manifest.get("layers")
        if not isinstance(layers, list) or not 1 <= len(layers) <= 128:
            raise GateError("invalid OCI layers")
        layer_paths = [blob_for(blobs, layer) for layer in layers]
        common = ["--repository", "m8-local.invalid/uptime-phoenix-probe", "--version", version,
                  "--architecture", arch, "--index", str(index_path),
                  "--index-sha256", match[1], "--index-size", str(index_path.stat().st_size),
                  "--manifest", str(manifest_path), "--config", str(config_path)]
        meta = strict_json(runner.call(arch + "-metadata", [str(admin), "verify-image", *common], work))
        content_args = [str(admin), "verify-image-content", *common]
        for layer in layer_paths:
            content_args.extend(["--layer", str(layer)])
        content = strict_json(runner.call(arch + "-content", content_args, work))
        for view in (meta, content):
            if (view.get("architecture") != arch or view.get("os") != "linux"
                    or view.get("manifest_sha256") != selected[0]["digest"][7:]
                    or view.get("config_sha256") != manifest["config"]["digest"][7:]
                    or view.get("index_sha256") != match[1]
                    or view.get("pinned_version") != version
                    or view.get("repository") != "m8-local.invalid/uptime-phoenix-probe"
                    or view.get("image_reference") != "m8-local.invalid/uptime-phoenix-probe@" + selected[0]["digest"]
                    or view.get("index_size") != index_path.stat().st_size
                    or view.get("version_source") != "digest_bound_config_label"
                    or type(view.get("layer_count")) is not int or view.get("layer_count") != len(layers)
                    or view.get("runtime_version_verified") is not False):
                raise GateError("CLI selection acknowledgement mismatch")
        if (meta.get("state") != "image_metadata_verified" or meta.get("layers_verified") is not False
                or content.get("state") != "image_content_digests_verified"
                or content.get("layer_digests_verified") is not True
                or content.get("diff_ids_verified") is not True
                or content.get("rootfs_verified") is not False
                or content.get("content_retained") is not False
                or type(content.get("layer_bytes")) is not int
                or content.get("layer_bytes") != sum(d["size"] for d in layers)
                or type(content.get("uncompressed_bytes")) is not int
                or not 0 <= content["uncompressed_bytes"] <= 2 * 1024 * 1024 * 1024):
            raise GateError("CLI verification/nonclaim acknowledgement mismatch")
        binary_path = work / ("." + arch + "-copied-probe")
        binary = copied_probe(layer_paths[-1], layers[-1]["mediaType"], binary_path, arch)
        binary_args = [str(admin), "verify-artifact", "--file", str(binary_path), "--version", version,
                       "--architecture", arch, "--sha256", binary["sha256"], "--size", str(binary["size"])]
        binary_view = strict_json(runner.call(arch + "-binary", binary_args, work))
        if (binary_view.get("state") != "artifact_verified" or binary_view.get("os") != "linux"
                or binary_view.get("architecture") != arch or binary_view.get("sha256") != binary["sha256"]
                or binary_view.get("size") != binary["size"] or binary_view.get("pinned_version") != version
                or binary_view.get("version_source") != "operator_pin"):
            raise GateError("copied binary acknowledgement mismatch")
        wrong_binary = binary_args.copy()
        wrong_binary[wrong_binary.index("--architecture") + 1] = "arm64" if arch == "amd64" else "amd64"
        output = runner.call(arch + "-reject-binary", wrong_binary, work, expect=1)
        if (output.strip() or b"probe artifact build metadata mismatch" not in
                (work / (arch + "-reject-binary.stderr")).read_bytes()):
            raise GateError("binary platform control did not refuse intended metadata")
        bad_config = work / (arch + "-corrupt-config")
        config_bytes = config_path.read_bytes()
        # Keep valid JSON and a syntactically valid diff ID. Metadata-only
        # verification does not compare diff IDs to layer bytes: its digest
        # check, not a JSON/label parser refusal, must catch this control.
        diff_id = read_json(config_path)["rootfs"]["diff_ids"][0]
        if not isinstance(diff_id, str) or not SHA.fullmatch(diff_id):
            raise GateError("config control requires a canonical diff ID")
        changed_id = diff_id[:7] + ("1" if diff_id[7] == "0" else "0") + diff_id[8:]
        needle = json.dumps(diff_id).encode()
        changed = config_bytes.replace(needle, json.dumps(changed_id).encode(), 1)
        if changed == config_bytes or len(changed) != len(config_bytes):
            raise GateError("config control did not change exact bytes")
        strict_json(changed)
        bad_config.write_bytes(changed)
        bad_common = common.copy()
        bad_common[bad_common.index("--config") + 1] = str(bad_config)
        output = runner.call(arch + "-reject-config", [str(admin), "verify-image", *bad_common], work, expect=1)
        expected_error = b"probe image metadata digest mismatch"
        if output.strip() or expected_error not in (work / (arch + "-reject-config.stderr")).read_bytes():
            raise GateError("config negative control did not refuse intended digest")
        bad_layer = work / (arch + "-corrupt-layer")
        with layer_paths[0].open("rb") as source, bad_layer.open("xb") as target:
            header = bytearray(source.read(10))
            if layers[0]["mediaType"].endswith("+gzip"):
                if len(header) < 10 or header[:2] != b"\x1f\x8b":
                    raise GateError("gzip control header missing")
                # MTIME is not part of decoded bytes or CRC: isolate the
                # compressed digest without creating an invalid codec stream.
                header[4] ^= 1
            else:
                header[0] ^= 1
            target.write(header)
            while chunk := source.read(65536):
                target.write(chunk)
        bad_args = content_args.copy()
        bad_args[bad_args.index("--layer") + 1] = str(bad_layer)
        output = runner.call(arch + "-reject-layer", bad_args, work, expect=1)
        if output.strip() or expected_error not in (work / (arch + "-reject-layer.stderr")).read_bytes():
            raise GateError("layer negative control did not refuse intended digest")
        results[arch] = {"manifest_sha256": selected[0]["digest"][7:],
                         "config_sha256": manifest["config"]["digest"][7:],
                         "layer_count": len(layers), "layer_bytes": content["layer_bytes"],
                         "uncompressed_bytes": content["uncompressed_bytes"],
                         "metadata_passed": True, "content_passed": True,
                         "copied_binary": binary, "copied_binary_platform_verified": True,
                         "wrong_binary_platform_refused": True,
                         "corrupt_config_refused": True, "corrupt_layer_refused": True}
    return {"local_index_sha256": match[1], "platforms": results,
            "publisher_verified": False, "registry_presence_verified": False,
            "rootfs_verified": False, "runtime_version_verified": False}


def run_gate(args) -> dict:
    if not args.allow_public_pulls:
        raise GateError("explicit public-pull approval required")
    if (not args.docker_endpoint.startswith("unix:///")
            or not stat.S_ISSOCK(Path(args.docker_endpoint[7:]).stat().st_mode)):
        raise GateError("available local Unix Docker socket required")
    root = Path(__file__).resolve().parents[1]
    work = Path(args.work_dir).absolute()
    work.mkdir(mode=0o700)
    scratch, config = work / ".scratch", work / ".docker"
    scratch.mkdir(mode=0o700)
    config.mkdir(mode=0o700)
    save(config / "config.json", {"cliPluginsExtraDirs": [str(Path(args.buildx_dir).resolve())]})
    env = isolated_env(scratch, config, args.docker_endpoint)
    runner = Runner(work, env, args.timeout)
    try:
        head = runner.call("source-head", ["git", "rev-parse", "HEAD"], root).decode().strip()
        version = "0.0.0-m8-" + head[:12]
        dirty = runner.call("source-diff", ["git", "diff", "--name-only", head, "--", "go.mod", "go.sum",
                                           ":(glob)**/*.go", ":(glob)**/*.sql"], root)
        untracked = runner.call("source-untracked", ["git", "ls-files", "--others", "--exclude-standard", "--",
                                                    ":(glob)**/*.go", ":(glob)**/*.sql"], root)
        if dirty.strip() or untracked.strip():
            raise GateError("Go/SQL/module source differs from tracked build snapshot")
        baseline = set(runner.call("baseline-containers", ["docker", "ps", "-aq", "--no-trunc"], root).split())
        inspect = runner.call("builder", ["docker", "buildx", "inspect"], root).decode()
        if not re.search(r"^Driver:\s+docker\s*$", inspect, re.MULTILINE):
            raise GateError("built-in Docker builder required; privileged helper creation refused")
        source_tar = work / "source.tar"
        runner.call("source-archive", ["git", "archive", "--format=tar", "--output=" + str(source_tar), head], root)
        context = work / ".context"
        snapshot_source(source_tar, context)
        dockerfile = context / "Dockerfile.probe"
        tracked = dockerfile.read_text(encoding="utf-8")
        original, overlay = choose_dockerfile(tracked, (root / "Dockerfile.probe").read_text(encoding="utf-8"),
                                             args.allow_working_dockerfile)
        refs = re.findall(r"^FROM (?:--platform=\$BUILDPLATFORM )?(\S+)(?: AS go-builder)?$", original, re.MULTILINE)
        if len(refs) != 2:
            raise GateError("unexpected source Dockerfile")
        # Validate public destinations before making any registry request.
        pinned_dockerfile(original, {ref: "sha256:" + "0" * 64 for ref in refs})
        pins = {}
        for i, ref in enumerate(refs):
            raw = runner.call("base-index-" + str(i), ["docker", "buildx", "imagetools", "inspect", "--raw", ref], root, limit=META_LIMIT)
            strict_json(raw)
            pins[ref] = "sha256:" + sha(raw)
        rendered = pinned_dockerfile(original, pins)
        dockerfile.write_text(rendered, encoding="utf-8")
        save(work / "source-pins.json", {"source_commit": head, "version": version,
             "tracked_dockerfile_sha256": sha(tracked.encode()),
             "original_dockerfile_sha256": sha(original.encode()),
             "pinned_dockerfile_sha256": sha(rendered.encode()), "base_indices": pins,
             "source_origin": "tracked_git_archive_plus_explicit_dockerfile_overlay" if overlay else "tracked_git_archive",
             "working_dockerfile_overlay": overlay, "credentials_forwarded": False})
        go_version = runner.call("go-version", [args.go, "version"], root).decode().strip()
        builder_version = re.search(r"golang:([0-9.]+)-alpine", original)
        if not builder_version or "go" + builder_version[1] + " " not in go_version + " ":
            raise GateError("host verifier toolchain differs from Dockerfile Go pin")
        admin = scratch / "phoenix-probe-admin"
        runner.call("build-verifier", [args.go, "build", "-o", str(admin), "./cmd/phoenix-probe-admin"], root)
        result = {"platforms": {}, "local_index_sha256_by_platform": {},
                  "publisher_verified": False, "registry_presence_verified": False,
                  "rootfs_verified": False, "runtime_version_verified": False}
        # Two Go cross-compilers can exhaust a small local Docker VM. Build
        # each real platform export sequentially; do not fabricate a combined
        # registry index or change global daemon memory/worker settings.
        for arch in ("amd64", "arm64"):
            platform_work = work / arch
            platform_work.mkdir(mode=0o700)
            export, metadata = platform_work / "probe.oci.tar", platform_work / "build-metadata.json"
            runner.call(arch + "-build-image", build_args(context, export, metadata, version,
                                                        platform="linux/" + arch), root)
            platform_runner = Runner(platform_work, env, args.timeout)
            try:
                observed = verify_export(platform_work, export, metadata, admin, version,
                                         platform_runner, architectures=(arch,))
            finally:
                runner.commands.extend(platform_runner.commands)
            result["platforms"].update(observed["platforms"])
            result["local_index_sha256_by_platform"][arch] = observed["local_index_sha256"]
        current = set(runner.call("final-containers", ["docker", "ps", "-aq", "--no-trunc"], root).split())
        if not baseline <= current:
            raise GateError("baseline container missing; cleanup authority not inferred")
        result.update({"passed": True, "source_commit": head, "version": version,
                       "go_version": go_version, "verifier_sha256": sha(admin.read_bytes()),
                       "gate_sha256": sha(Path(__file__).read_bytes()),
                       "image_loaded_or_run": False, "image_published": False,
                       "shared_builder_cache_pruned": False, "baseline_containers_preserved": True})
        save(work / "result.json", result)
        return result
    finally:
        save(work / "commands.json", {"commands": runner.commands})


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-public-pulls", action="store_true")
    parser.add_argument("--allow-working-dockerfile", action="store_true",
                        help="explicitly overlay the current tracked Dockerfile for a local uncommitted fix")
    parser.add_argument("--docker-endpoint", required=True)
    parser.add_argument("--buildx-dir", required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--work-dir", required=True, help="new private directory; must not exist")
    parser.add_argument("--timeout", type=int, default=2400, help="per local client seconds, 60–7200")
    args = parser.parse_args()
    os.umask(0o077)
    if not 60 <= args.timeout <= 7200:
        parser.error("timeout must be 60–7200 seconds")
    try:
        result = run_gate(args)
    except KeyboardInterrupt:
        print(json.dumps({"passed": False, "error": "local correspondence gate interrupted"}))
        return 130
    except GateError as exc:
        print(json.dumps({"passed": False, "error": str(exc)}))
        return 1
    except (OSError, ValueError, KeyError, TypeError, EOFError, tarfile.TarError, subprocess.SubprocessError):
        print(json.dumps({"passed": False, "error": "local correspondence gate unavailable or malformed input"}))
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
