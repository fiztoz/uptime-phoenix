"""Offline check that Makefile recipes only name Go packages that exist (#176)."""

from pathlib import Path
import re
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


if __name__ == "__main__":
    unittest.main()
