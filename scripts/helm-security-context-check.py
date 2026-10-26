#!/usr/bin/env python3
"""Assert every rendered workload runs non-root under RuntimeDefault seccomp.

Consumes `helm template` output on stdin (issue #91). For each Deployment,
StatefulSet, Job, CronJob and Pod it requires:
  - a pod-level seccompProfile of type RuntimeDefault;
  - runAsNonRoot: true at pod or container level;
  - on every container and init container: allowPrivilegeEscalation: false
    and capabilities.drop containing ALL.

Subchart manifests (`# Source: <chart>/charts/...`, e.g. the optional Valkey
dependency) are skipped: their security settings come from the subchart's own
values, not from this chart's templates.

No YAML dependency: the check walks indentation, which is stable for Helm's
rendered output.
"""

import re
import sys

WORKLOAD_KINDS = {"Deployment", "StatefulSet", "Job", "CronJob", "Pod"}


def indent(line):
    return len(line) - len(line.lstrip(" "))


def container_blocks(lines):
    """Yield (name, text) for each item under containers/initContainers."""
    i = 0
    while i < len(lines):
        m = re.match(r"^(\s*)(containers|initContainers):\s*$", lines[i])
        if not m:
            i += 1
            continue
        base = len(m.group(1))
        j = i + 1
        items = []
        current = None
        while j < len(lines):
            line = lines[j]
            if line.strip() == "":
                j += 1
                continue
            ind = indent(line)
            if ind < base or (ind == base and not line.lstrip().startswith("- ")):
                break
            if line.lstrip().startswith("- ") and (current is None or ind <= current[0]):
                current = [ind, [line]]
                items.append(current)
            elif current is not None:
                current[1].append(line)
            j += 1
        for _, body in items:
            text = "\n".join(body)
            name = re.search(r"name:\s*(\S+)", text)
            yield (name.group(1) if name else "?"), text
        i = j


def check(rendered):
    problems = []
    seen = 0
    for doc in re.split(r"^---\s*$", rendered, flags=re.MULTILINE):
        kind = re.search(r"^kind: (\S+)$", doc, re.MULTILINE)
        if not kind or kind.group(1) not in WORKLOAD_KINDS:
            continue
        if re.search(r"^# Source: [^/]+/charts/", doc, re.MULTILINE):
            continue
        seen += 1
        name = re.search(r"^  name: (\S+)$", doc, re.MULTILINE)
        label = f"{kind.group(1)}/{name.group(1) if name else '?'}"
        if not re.search(r"seccompProfile:\s*\n\s+type: RuntimeDefault", doc):
            problems.append(f"{label}: no seccompProfile RuntimeDefault")
        lines = doc.splitlines()
        blocks = list(container_blocks(lines))
        if not blocks:
            problems.append(f"{label}: no containers found")
        pod_non_root = re.search(r"^\s+securityContext:\s*\n(?:\s+\S.*\n)*?\s+runAsNonRoot: true", doc)
        for cname, text in blocks:
            where = f"{label} container {cname}"
            if "allowPrivilegeEscalation: false" not in text:
                problems.append(f"{where}: allowPrivilegeEscalation is not false")
            if not re.search(r"drop:\s*\n\s+- ALL", text):
                problems.append(f"{where}: capabilities.drop does not include ALL")
            if "runAsNonRoot: true" not in text and not pod_non_root:
                problems.append(f"{where}: runAsNonRoot is not set")
    if seen == 0:
        problems.append("no workloads rendered")
    return problems


def main():
    problems = check(sys.stdin.read())
    for p in problems:
        print(f"SECURITY CONTEXT: {p}", file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
