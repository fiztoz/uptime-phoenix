#!/usr/bin/env python3
"""Verify the actual release workflow blocks fan-in when chart publication fails."""

import os
from pathlib import Path
import subprocess
import tempfile
import textwrap


def main():
    root = Path(__file__).resolve().parents[2]
    lines = (root / ".github/workflows/release.yml").read_text().splitlines()
    header = lines.index("      - name: Package and push Helm chart to OCI")
    start = lines.index("        run: |", header) + 1
    end = start
    while end < len(lines) and (not lines[end] or lines[end].startswith("          ")):
        end += 1
    script = textwrap.dedent("\n".join(lines[start:end]))
    if "helm push" not in script or "set -euo pipefail" not in script:
        raise SystemExit("release chart step is missing its required shell contract")

    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="phoenix-chart-publish-") as directory:
        temporary = Path(directory)
        fake_bin = temporary / "bin"
        fake_bin.mkdir()
        helm = fake_bin / "helm"
        helm.write_text(
            "#!/usr/bin/env bash\nset -euo pipefail\n"
            "if [[ ${1:-} == show && ${2:-} == chart ]]; then\n"
            "  printf 'version: 0.5.0-rc.1\\nappVersion: 0.5.0-rc.1\\n'\n"
            "elif [[ ${1:-} == push && ${3:-} == oci://ghcr.io/fixture/charts ]]; then\n"
            "  printf 'push\\n' >> \"$PUSH_LOG\"\n"
            "  exit \"$PUSH_EXIT\"\n"
            "else\n  exit 99\nfi\n"
        )
        helm.chmod(0o700)
        charts = temporary / "dist/release-0.5.0-rc.1/charts"
        charts.mkdir(parents=True)
        (charts / "uptime-phoenix-0.5.0-rc.1.tgz").write_bytes(b"mock chart package")
        run_script = temporary / "chart-step.sh"
        run_script.write_text(script + "\nprintf 'chart-step-complete\\n'\n")
        for name, expected in (("chart-push-success", 0), ("chart-push-failure", 42)):
            push_log = temporary / (name + ".log")
            result = subprocess.run(
                ["bash", str(run_script)], cwd=temporary, text=True, capture_output=True,
                timeout=15, env={**os.environ, "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                                 "VERSION": "0.5.0-rc.1", "IMAGE_OWNER": "fixture",
                                 "PUSH_LOG": str(push_log), "PUSH_EXIT": str(expected)},
            )
            completed = "chart-step-complete" in result.stdout
            if (result.returncode != expected or completed != (expected == 0) or
                    not push_log.exists() or push_log.read_text() != "push\n"):
                raise SystemExit(name + " did not enforce the selected chart publication result")
            print(f"PASS {name} exit={result.returncode}")


if __name__ == "__main__":
    main()
