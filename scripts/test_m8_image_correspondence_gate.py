"""Fast isolated controls; mocked CLI/archive cases are not actual image acceptance."""
import argparse
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import m8_image_correspondence_gate as gate


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


def write_tar(path, entries):
    with tarfile.open(path, "w") as archive:
        for name, data, kind in entries:
            member = tarfile.TarInfo(name)
            member.type = kind
            if kind == tarfile.REGTYPE:
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
            else:
                member.linkname = "../outside"
                archive.addfile(member)


def fixture(work, *, architectures=("amd64", "arm64"), wrong_pin=False, wrong_binary=False):
    blobs, manifests = {}, []

    def blob(data, media):
        digest = gate.sha(data)
        blobs[digest] = data
        return {"mediaType": media, "digest": "sha256:" + digest, "size": len(data)}

    for arch in architectures:
        header = bytearray(64)
        header[:6] = b"\x7fELF\x02\x01"
        header[18:20] = (62 if wrong_binary else {"amd64": 62, "arm64": 183}[arch]).to_bytes(2, "little")
        layer_bytes = io.BytesIO()
        with tarfile.open(fileobj=layer_bytes, mode="w") as tar:
            member = tarfile.TarInfo("uptime-phoenix-probe")
            member.size = len(header)
            tar.addfile(member, io.BytesIO(header))
        layer = blob(layer_bytes.getvalue(), "application/vnd.oci.image.layer.v1.tar")
        config = blob(encoded({"architecture": arch, "os": "linux",
                               "rootfs": {"type": "layers", "diff_ids": ["sha256:" + gate.sha(layer_bytes.getvalue())]}}),
                      "application/vnd.oci.image.config.v1+json")
        manifest = blob(encoded({"schemaVersion": 2, "mediaType": gate.MANIFEST_MEDIA,
                                 "config": config, "layers": [layer]}), gate.MANIFEST_MEDIA)
        manifest["platform"] = {"os": "linux", "architecture": arch}
        manifests.append(manifest)
    index = blob(encoded({"schemaVersion": 2, "mediaType": gate.INDEX_MEDIA,
                          "manifests": manifests}), gate.INDEX_MEDIA)
    wrapper = encoded({"schemaVersion": 2, "mediaType": gate.INDEX_MEDIA, "manifests": [index]})
    entries = [("oci-layout", encoded({"imageLayoutVersion": "1.0.0"}), tarfile.REGTYPE),
               ("index.json", wrapper, tarfile.REGTYPE)]
    entries += [("blobs/sha256/" + digest, data, tarfile.REGTYPE) for digest, data in blobs.items()]
    archive = work / "export.tar"
    write_tar(archive, entries)
    metadata = work / "metadata.json"
    metadata.write_bytes(encoded({"containerimage.digest": "sha256:" + ("0" * 64 if wrong_pin else index["digest"][7:])}))
    return archive, metadata


class FakeCLI:
    """Only tests gate plumbing/ack validation; never counts as image execution."""

    def __init__(self, work, mutate=None, refused=True):
        self.work, self.mutate, self.refused, self.calls = work, mutate, refused, []

    def call(self, label, args, cwd, *, expect=0, **_):
        self.calls.append(label)
        if expect:
            (self.work / (label + ".stderr")).write_bytes(
                (b"probe artifact build metadata mismatch\n" if "reject-binary" in label else
                 b"probe image metadata digest mismatch\n") if self.refused else b"wrong error\n")
            return b"" if self.refused else b'{"state":"unexpected-success"}'
        values = dict(zip(args[2::2], args[3::2]))
        if args[1] == "verify-artifact":
            return encoded({"state": "artifact_verified", "os": "linux", "architecture": values["--architecture"],
                            "pinned_version": values["--version"], "version_source": "operator_pin",
                            "sha256": values["--sha256"], "size": int(values["--size"])})
        manifest = gate.read_json(Path(values["--manifest"]))
        result = {"architecture": values["--architecture"], "os": "linux",
                  "pinned_version": values["--version"], "version_source": "digest_bound_config_label",
                  "repository": values["--repository"], "index_sha256": values["--index-sha256"],
                  "index_size": int(values["--index-size"]),
                  "manifest_sha256": gate.sha(Path(values["--manifest"]).read_bytes()),
                  "config_sha256": manifest["config"]["digest"][7:],
                  "image_reference": values["--repository"] + "@sha256:" + gate.sha(Path(values["--manifest"]).read_bytes()),
                  "layer_count": len(manifest["layers"]), "runtime_version_verified": False}
        if args[1] == "verify-image":
            result.update(state="image_metadata_verified", layers_verified=False)
        else:
            result.update(state="image_content_digests_verified", layer_digests_verified=True,
                          diff_ids_verified=True, rootfs_verified=False, content_retained=False,
                          layer_bytes=sum(d["size"] for d in manifest["layers"]), uncompressed_bytes=20480)
        if self.mutate:
            self.mutate(label, result)
        return encoded(result)


class JSONAndNames(unittest.TestCase):
    def test_strict_json(self):
        self.assertEqual(gate.strict_json(b'{"ok":1}'), {"ok": 1})
        for value in (b'[]', b'null', b'{"a":1,"a":2}', b'{"a":1,"\\u0061":2}',
                      b'{"a":NaN}', b'{} {}', b'{"a":"\xff"}'):
            with self.subTest(value=value), self.assertRaises(gate.GateError):
                gate.strict_json(value)
        with self.assertRaises(gate.GateError):
            gate.strict_json(b" " * (gate.META_LIMIT + 1))

    def test_archive_names(self):
        self.assertEqual(gate.safe_name("blobs/sha256/x"), "blobs/sha256/x")
        self.assertEqual(gate.safe_name("blobs/", True), "blobs")
        for value in ("", "/outside", "../outside", "a/../b", "a/./b", "a//b",
                      "./a", "a\\b", "a\0b", "x" * 513):
            with self.subTest(value=value), self.assertRaises(gate.GateError):
                gate.safe_name(value)

    def test_dockerfile_pins_only(self):
        original = ("FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS go-builder\n"
                    "RUN echo literal\nFROM gcr.io/distroless/static-debian12:nonroot\nUSER 65532\n")
        pins = {"golang:1.26.9-alpine": "sha256:" + "a" * 64,
                "gcr.io/distroless/static-debian12:nonroot": "sha256:" + "b" * 64}
        rendered = gate.pinned_dockerfile(original, pins)
        for ref, digest in pins.items():
            rendered = rendered.replace(ref + "@" + digest, ref)
        self.assertEqual(rendered, original)
        for bad in ({}, {**pins, "extra": "sha256:" + "c" * 64},
                    {**pins, "golang:1.26.9-alpine": "sha256:bad"}):
            with self.subTest(bad=bad), self.assertRaises(gate.GateError):
                gate.pinned_dockerfile(original, bad)
        with self.assertRaises(gate.GateError):
            gate.pinned_dockerfile(original.replace("golang:1.26.9-alpine", "internal.invalid/private"), pins)

    def test_from_pins_cannot_be_intercepted_by_comments(self):
        original = ("# golang:1.26.9-alpine is the native builder\n"
                    "# gcr.io/distroless/static-debian12:nonroot is the runtime\n"
                    "FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS go-builder\n"
                    "FROM gcr.io/distroless/static-debian12:nonroot\n")
        pins = {"golang:1.26.9-alpine": "sha256:" + "a" * 64,
                "gcr.io/distroless/static-debian12:nonroot": "sha256:" + "b" * 64}
        rendered = gate.pinned_dockerfile(original, pins)
        self.assertEqual(rendered.splitlines()[:2], original.splitlines()[:2])
        for ref, digest in pins.items():
            self.assertTrue(any(line.startswith("FROM ") and ref + "@" + digest in line
                                for line in rendered.splitlines()), "actual FROM was not pinned")

    def test_working_dockerfile_requires_explicit_overlay(self):
        self.assertEqual(gate.choose_dockerfile("same", "same", False), ("same", False))
        with self.assertRaises(gate.GateError):
            gate.choose_dockerfile("tracked", "working", False)
        self.assertEqual(gate.choose_dockerfile("tracked", "working", True), ("working", True))

    def test_probe_automatic_target_arguments_have_no_defaults(self):
        dockerfile = (Path(__file__).resolve().parents[1] / "Dockerfile.probe").read_text()
        for name in ("TARGETOS", "TARGETARCH"):
            with self.subTest(name=name):
                self.assertIn("ARG " + name + "\n", dockerfile,
                              "automatic target defaults override the requested BuildKit platform")
                self.assertNotIn("ARG " + name + "=", dockerfile)

    def test_isolated_environment(self):
        with mock.patch.dict(os.environ, {"TEST_MARIADB_DSN": "private-marker", "SECRET_KEY": "private-marker",
                                        "DOCKER_AUTH_CONFIG": "private-marker", "DOCKER_CONTEXT": "foreign",
                                        "HTTP_PROXY": "private-marker", "GOFLAGS": "private-marker"}):
            env = gate.isolated_env(Path("/scratch"), Path("/config"), "unix:///local.sock")
        self.assertNotIn("private-marker", env.values())
        for key in ("TEST_MARIADB_DSN", "SECRET_KEY", "DOCKER_AUTH_CONFIG", "DOCKER_CONTEXT", "HTTP_PROXY", "GOFLAGS"):
            self.assertNotIn(key, env)
        self.assertEqual(env["GOPROXY"], "off")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["DOCKER_CONFIG"], "/config")

    def test_build_has_no_publish_load_or_host_effects(self):
        args = gate.build_args(Path("/copy"), Path("/out.tar"), Path("/meta"), "0.0.0-m8-test")
        for forbidden in ("--push", "--load", "--privileged", "--ssh", "--secret", "--network=host"):
            self.assertNotIn(forbidden, args)
        self.assertIn("linux/amd64,linux/arm64", args)
        self.assertIn("--no-cache", args)
        self.assertTrue(any(x.startswith("type=oci,") for x in args))

    def test_missing_approval_and_nonlocal_refuse_before_io(self):
        with self.assertRaises(gate.GateError):
            gate.run_gate(argparse.Namespace(allow_public_pulls=False))
        with self.assertRaises(gate.GateError):
            gate.run_gate(argparse.Namespace(allow_public_pulls=True, docker_endpoint="tcp://internal.invalid:2376"))


class Archives(unittest.TestCase):
    def test_oci_exact_digest_and_layout(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, _ = fixture(work)
            blobs, metadata = gate.unpack_oci(archive, work / ".blobs")
            self.assertGreaterEqual(len(blobs), 7)
            self.assertEqual(set(metadata), {"oci-layout", "index.json"})
            self.assertTrue(all(gate.sha(p.read_bytes()) == key for key, p in blobs.items()))

    def test_oci_unsafe_members_duplicate_hash_and_limits(self):
        good = [("oci-layout", encoded({"imageLayoutVersion": "1.0.0"}), tarfile.REGTYPE),
                ("index.json", b"{}", tarfile.REGTYPE)]
        cases = [good + [("../outside", b"x", tarfile.REGTYPE)],
                 good + [("blobs/sha256/" + "a" * 64, b"wronghash", tarfile.REGTYPE)],
                 good + [("link", b"", tarfile.SYMTYPE)],
                 good + [("link", b"", tarfile.LNKTYPE)],
                 good + [("pipe", b"", tarfile.FIFOTYPE)],
                 good + [("index.json", b"{}", tarfile.REGTYPE)],
                 good + [("unexpected", b"x", tarfile.REGTYPE)],
                 [("index.json", b"{}", tarfile.REGTYPE)]]
        for i, entries in enumerate(cases):
            with self.subTest(case=i), tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp)
                write_tar(path / "in.tar", entries)
                with self.assertRaises(gate.GateError):
                    gate.unpack_oci(path / "in.tar", path / ".blobs")
                self.assertFalse((path.parent / "outside").exists())
        for limits in ({"entry_limit": 1}, {"total_limit": 1}, {"blob_limit": 1}):
            with self.subTest(limits=limits), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                archive, _ = fixture(work)
                with self.assertRaises(gate.GateError):
                    gate.unpack_oci(archive, work / ".blobs", **limits)

    def test_source_refuses_links_and_duplicate(self):
        for entries in ([("link", b"", tarfile.SYMTYPE)],
                        [("../outside", b"x", tarfile.REGTYPE)],
                        [("a", b"1", tarfile.REGTYPE), ("a", b"2", tarfile.REGTYPE)]):
            with self.subTest(entries=entries), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                write_tar(work / "source.tar", entries)
                with self.assertRaises(gate.GateError):
                    gate.snapshot_source(work / "source.tar", work / ".context")

    def test_git_snapshot_excludes_untracked_and_private_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            repo = work / "repo"
            repo.mkdir()
            env = {"PATH": os.environ["PATH"], "HOME": str(work),
                   "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}
            def git(*args):
                subprocess.run(["git", *args], cwd=repo, env=env, check=True,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            git("init")
            (repo / "tracked.go").write_text("package fixture\n")
            git("add", "tracked.go")
            git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
            (repo / ".env").write_text("private-marker")
            (repo / "untracked.go").write_text("package private\n")
            git("archive", "--format=tar", "--output=" + str(work / "source.tar"), "HEAD")
            gate.snapshot_source(work / "source.tar", work / ".context")
            self.assertEqual({p.name for p in (work / ".context").iterdir()}, {"tracked.go"})


class ExportAcknowledgements(unittest.TestCase):
    def test_both_platforms_and_all_controls_execute(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work)
            cli = FakeCLI(work)
            result = gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", cli)
            self.assertEqual(set(result["platforms"]), {"amd64", "arm64"})
            self.assertEqual(len(cli.calls), 12)
            self.assertTrue(all(v["copied_binary_platform_verified"] for v in result["platforms"].values()))
            self.assertFalse(result["runtime_version_verified"])
            self.assertFalse(result["publisher_verified"])

    def test_sequential_single_platform_export_executes_all_controls(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work, architectures=("arm64",))
            cli = FakeCLI(work)
            result = gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", cli,
                                        architectures=("arm64",))
            self.assertEqual(set(result["platforms"]), {"arm64"})
            self.assertEqual(len(cli.calls), 6)
            args = gate.build_args(work, work / "out", work / "meta", "0.0.0-test", platform="linux/arm64")
            self.assertEqual(args[args.index("--platform") + 1], "linux/arm64")

    def test_arm64_label_cannot_hide_amd64_elf(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work, wrong_binary=True)
            cli = FakeCLI(work)
            with self.assertRaisesRegex(gate.GateError, "ELF architecture mismatch"):
                gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", cli)
            self.assertIn("arm64-content", cli.calls)
            self.assertNotIn("arm64-binary", cli.calls)

    def test_final_copy_refuses_links_paths_and_missing_binary(self):
        for entries in ([("uptime-phoenix-probe", b"", tarfile.SYMTYPE)],
                        [("../uptime-phoenix-probe", b"x" * 64, tarfile.REGTYPE)],
                        [("other", b"x" * 64, tarfile.REGTYPE)], []):
            with self.subTest(entries=entries), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                write_tar(work / "layer", entries)
                with self.assertRaises(gate.GateError):
                    gate.copied_probe(work / "layer", "application/vnd.oci.image.layer.v1.tar", work / ".copy", "amd64")
                self.assertFalse((work / ".copy").exists())

    def test_copied_binary_acknowledgement_must_match(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work)
            cli = FakeCLI(work)
            original = cli.call
            def foreign(label, args, cwd, **kwargs):
                result = original(label, args, cwd, **kwargs)
                if label.endswith("-binary") and not kwargs.get("expect"):
                    view = json.loads(result)
                    view["architecture"] = "foreign"
                    return encoded(view)
                return result
            cli.call = foreign
            with self.assertRaisesRegex(gate.GateError, "binary acknowledgement"):
                gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", cli)

    def test_missing_platform_and_changed_build_pin_refuse(self):
        for options in ({"architectures": ("amd64",)}, {"wrong_pin": True},
                        {"architectures": ("amd64", "amd64", "arm64")}):
            with self.subTest(options=options), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                archive, metadata = fixture(work, **options)
                with self.assertRaises(gate.GateError):
                    gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", FakeCLI(work))

    def test_foreign_missing_malformed_acknowledgements_refuse(self):
        cases = {"architecture": "foreign", "pinned_version": "foreign", "index_sha256": "0" * 64,
                 "manifest_sha256": "0" * 64, "config_sha256": "0" * 64, "repository": "foreign",
                 "image_reference": "foreign", "version_source": "operator_pin", "index_size": 1,
                 "layer_count": True, "runtime_version_verified": True, "state": "installed",
                 "layer_digests_verified": False, "diff_ids_verified": False, "rootfs_verified": True,
                 "content_retained": True, "layer_bytes": -1, "uncompressed_bytes": True}
        for field, value in cases.items():
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                archive, metadata = fixture(work)
                def mutate(label, result):
                    if label.endswith("-content"):
                        result[field] = value
                with self.assertRaises(gate.GateError):
                    gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", FakeCLI(work, mutate))
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work)
            def remove(_, result):
                result.pop("runtime_version_verified", None)
            with self.assertRaises(gate.GateError):
                gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", FakeCLI(work, remove))

    def test_wrong_negative_control_error_or_success_refuses(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            archive, metadata = fixture(work)
            with self.assertRaises(gate.GateError):
                gate.verify_export(work, archive, metadata, Path("/verifier"), "0.0.0-test", FakeCLI(work, refused=False))


class LocalClient(unittest.TestCase):
    def test_expected_exit_and_private_error_secrecy(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            runner = gate.Runner(work, {"PATH": os.environ["PATH"]}, 3)
            script = "import sys;sys.stderr.write('private-marker');sys.exit(2)"
            with self.assertRaises(gate.GateError) as caught:
                runner.call("refused", [sys.executable, "-c", script], work, expect=1)
            self.assertNotIn("private-marker", str(caught.exception))
            self.assertEqual((work / "refused.exit").read_text(), "2")

    def test_timeout_joins_local_client(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            runner = gate.Runner(work, {"PATH": os.environ["PATH"]}, 0.05)
            with self.assertRaisesRegex(gate.GateError, "timeout"):
                runner.call("timeout", [sys.executable, "-c", "import time; time.sleep(5)"], work)
            self.assertNotEqual((work / "timeout.exit").read_text(), "None")

    def test_output_budget_refuses_and_joins(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            runner = gate.Runner(work, {"PATH": os.environ["PATH"]}, 3)
            with self.assertRaisesRegex(gate.GateError, "log limit"):
                runner.call("budget", [sys.executable, "-c", "print('x'*10000)"], work, limit=100)
            self.assertNotEqual((work / "budget.exit").read_text(), "None")


if __name__ == "__main__":
    unittest.main()
