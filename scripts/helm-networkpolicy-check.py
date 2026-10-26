#!/usr/bin/env python3
"""Assert in-release NetworkPolicy egress ports are scoped to this release.

Consumes `helm template` output on stdin. Every egress rule of this chart's
NetworkPolicies that opens an in-cluster-only port (the API's 3000 or the
web container port 8080) must carry a `to:` selector; an unscoped rule
would open that port to every destination. Ports 53/80/443 (DNS and HTTP
monitor checks), the database, the event bus and cloudflared's 7844 edge
port are destination-agnostic by design and are not checked.

No YAML dependency: the check walks the rendered text.
"""

import re
import sys

IN_RELEASE_PORTS = {"3000", "8080"}


def documents(text):
    doc = []
    for line in text.splitlines():
        if line.strip() == "---":
            if doc:
                yield doc
            doc = []
            continue
        doc.append(line)
    if doc:
        yield doc


def egress_rules(doc):
    """Yield the text lines of each item under spec.egress."""
    for i, line in enumerate(doc):
        m = re.match(r"^(\s*)egress:\s*$", line)
        if not m:
            continue
        base = len(m.group(1))
        rule = None
        for nxt in doc[i + 1:]:
            if not nxt.strip() or nxt.lstrip().startswith("#"):
                continue
            ind = len(nxt) - len(nxt.lstrip(" "))
            if ind <= base and not nxt.lstrip().startswith("- "):
                break
            if ind <= base + 2 and nxt.lstrip().startswith("- "):
                if rule is not None:
                    yield rule
                rule = [nxt]
            elif rule is not None:
                rule.append(nxt)
        if rule is not None:
            yield rule


def main():
    errors = []
    policies = 0
    for doc in documents(sys.stdin.read()):
        src = next((l for l in doc if l.startswith("# Source:")), "")
        if "/charts/" in src or not any(re.match(r"^kind:\s*NetworkPolicy", l) for l in doc):
            continue
        policies += 1
        for rule in egress_rules(doc):
            text = "\n".join(rule)
            ports = set(re.findall(r"port:\s*(\d+)", text))
            if ports & IN_RELEASE_PORTS and not re.search(r"^\s*-?\s*to:", text, re.M):
                errors.append(f"{src}: egress rule opens {sorted(ports & IN_RELEASE_PORTS)} to every destination")
    if policies == 0:
        errors.append("no NetworkPolicy rendered; enable networkPolicy.enabled for this check")
    for e in errors:
        print("FAIL:", e, file=sys.stderr)
    if not errors:
        print(f"ok: {policies} NetworkPolicy object(s); in-release egress ports are scoped")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
