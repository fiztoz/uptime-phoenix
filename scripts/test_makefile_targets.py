"""Offline checks for Makefile package paths and linter bootstrap."""

import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
GO_PACKAGE = re.compile(r"\bgo\s+(?:run|build|install)\b[^\n]*?\s(\./cmd/[A-Za-z0-9_.-]+)")


def recipe_packages(makefile_text):
    return sorted(set(GO_PACKAGE.findall(makefile_text)))


class MakefileTargets(unittest.TestCase):
    def test_pattern_finds_go_run_and_build_paths(self):
        text = "a:\n\tgo run ./cmd/one up\nb:\n\tCGO_ENABLED=0 go build -o bin/x ./cmd/two\n"
        self.assertEqual(recipe_packages(text), ["./cmd/one", "./cmd/two"])

    def test_every_cmd_package_in_the_makefile_exists(self):
        packages = recipe_packages((ROOT / "Makefile").read_text())
        self.assertIn("./cmd/app", packages)
        missing = [p for p in packages if not (ROOT / p).is_dir()]
        self.assertEqual(missing, [], f"Makefile names missing Go packages: {missing}")


class LinterBootstrap(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.work = Path(self.directory.name)
        self.gopath = self.work / "gopath"
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.log = self.work / "calls"
        self.linter = self.gopath / "bin" / "golangci-lint"
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.version = re.search(
            r"uses: golangci/golangci-lint-action@[^\n]+\n\s+with:\n"
            r"(?:\s+#[^\n]*\n)*\s+version: (v[0-9.]+)", workflow
        ).group(1)
        self.install = (
            "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"
            + self.version
        )
        fake_go = self.bin / "go"
        fake_go.write_text(
            f"#!{sys.executable}\n"
            "import os, pathlib, sys\n"
            "args = sys.argv[1:]\n"
            f"gopath = pathlib.Path({str(self.gopath)!r})\n"
            f"log = pathlib.Path({str(self.log)!r})\n"
            "if args == ['env', 'GOPATH']:\n"
            "    print(gopath)\n"
            f"elif args == ['install', {self.install!r}]:\n"
            "    log.write_text('install ' + args[1] + '\\n')\n"
            "    binary = gopath / 'bin/golangci-lint'\n"
            "    binary.parent.mkdir(parents=True, exist_ok=True)\n"
            "    binary.write_text('#!/bin/sh\\nprintf \\\"lint %s\\\\n\\\" \\\"$*\\\" >> \\\"$QA_LINT_LOG\\\"\\n')\n"
            "    binary.chmod(0o755)\n"
            "else:\n"
            "    sys.exit('unexpected go invocation: ' + repr(args))\n"
        )
        fake_go.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        QA_LINT_LOG=str(self.log))

    def make(self, *args):
        result = subprocess.run(
            ["make", "--no-print-directory", "-f", str(ROOT / "Makefile"), *args],
            cwd=self.work, env=self.env, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_missing_linter_is_installed_then_run(self):
        self.make("lint-backend")
        self.assertEqual(self.log.read_text().splitlines(), ["install " + self.install, "lint run"])

    def test_existing_linter_is_reused(self):
        self.make("lint-backend")
        self.log.write_text("")
        self.make("lint-backend")
        self.assertEqual(self.log.read_text(), "lint run\n")

    def test_full_gate_uses_the_same_v2_installer(self):
        output = self.make("--dry-run", "MAKE=true", "gate-full")
        self.assertIn("go install " + self.install, output)
        self.assertNotIn("github.com/golangci/golangci-lint/cmd/", output)

    def test_documented_install_matches_ci(self):
        guide = (ROOT / "docs/TESTING.md").read_text()
        self.assertIn("go install " + self.install, guide)
        self.assertNotIn("github.com/golangci/golangci-lint/cmd/", guide)


if __name__ == "__main__":
    unittest.main()
