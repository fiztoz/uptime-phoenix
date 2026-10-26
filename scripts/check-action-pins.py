#!/usr/bin/env python3
"""Fail when a GitHub Actions `uses:` reference is not pinned to a commit SHA.

A tag such as `actions/checkout@v7` can be moved by whoever controls the
action's repository; a 40-character commit SHA cannot. Local actions
(`./...`) and `docker://` images pinned by digest are allowed.
"""

import pathlib
import re
import sys

USES = re.compile(r"^\s*(?:-\s*)?uses:\s*['\"]?([^'\"\s#]+)")
SHA = re.compile(r"^[^@\s]+@[0-9a-f]{40}$")


def problems(root: pathlib.Path) -> list[str]:
    found = []
    files = sorted(root.glob(".github/workflows/*.y*ml")) + sorted(root.glob(".github/actions/**/*.y*ml"))
    for path in files:
        for lineno, line in enumerate(path.read_text().splitlines(), 1):
            m = USES.match(line)
            if not m:
                continue
            ref = m.group(1)
            if ref.startswith("./"):
                continue
            if ref.startswith("docker://"):
                if "@sha256:" not in ref:
                    found.append(f"{path}:{lineno}: {ref} (pin the image by digest)")
                continue
            if not SHA.match(ref):
                found.append(f"{path}:{lineno}: {ref} (pin to a full commit SHA)")
    return found


def main() -> int:
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
    bad = problems(root)
    for line in bad:
        print(line)
    if bad:
        print(f"{len(bad)} unpinned action reference(s)")
        return 1
    print("all action references are pinned to commit SHAs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
