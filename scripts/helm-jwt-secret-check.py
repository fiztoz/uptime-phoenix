#!/usr/bin/env python3
"""Assert every Phoenix server Deployment gets JWT_SECRET from the same Secret.

Consumes `helm template` output on stdin. Every Phoenix binary (all-in-one,
api, worker) loads the same Config, and LoadConfig refuses to start without a
strong JWT_SECRET, so a Deployment running one of them without the key
crash-loops. This check finds each Deployment from this chart whose containers
include an `uptime-phoenix` container other than the web tier and requires:
  - a JWT_SECRET env entry on that container, from valueFrom.secretKeyRef;
  - the same secretKeyRef name/key on every such Deployment (the worker and
    the API must share one signing key).

Pass --expect N to also require that exactly N such Deployments rendered, so a
split render cannot pass by silently dropping the worker.

No YAML dependency: the check walks the rendered text, which is stable for
Helm's output.
"""

import re
import sys


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


def source(doc):
    for line in doc:
        m = re.match(r"^# Source: (\S+)", line)
        if m:
            return m.group(1)
    return ""


def kind(doc):
    for line in doc:
        m = re.match(r"^kind:\s*(\S+)", line)
        if m:
            return m.group(1)
    return ""


def server_containers(doc):
    """Yield (name, lines) for each uptime-phoenix server container."""
    for i, line in enumerate(doc):
        m = re.match(r"^(\s*)- name:\s*(uptime-phoenix(?:-api|-worker)?)\s*$", line)
        if not m:
            continue
        # A container list item has `image:` as a sibling key.
        base = len(m.group(1))
        block = [line]
        for nxt in doc[i + 1:]:
            ind = len(nxt) - len(nxt.lstrip(" "))
            if nxt.strip() and ind <= base:
                break
            block.append(nxt)
        if any(re.match(r"^\s*image:", b) for b in block):
            yield m.group(2), block


def jwt_ref(block):
    for i, line in enumerate(block):
        if re.match(r"^\s*- name:\s*JWT_SECRET\s*$", line):
            window = block[i + 1:i + 6]
            text = "\n".join(window)
            if "secretKeyRef:" not in text:
                return ("<not a secretKeyRef>", "")
            name = re.search(r"^\s*name:\s*(\S+)\s*$", text, re.M)
            key = re.search(r"^\s*key:\s*(\S+)\s*$", text, re.M)
            return (name.group(1) if name else "", key.group(1) if key else "")
    return None


def main():
    expect = None
    if len(sys.argv) == 3 and sys.argv[1] == "--expect":
        expect = int(sys.argv[2])
    errors = []
    refs = {}
    for doc in documents(sys.stdin.read()):
        src = source(doc)
        if kind(doc) != "Deployment" or "/charts/" in src:
            continue
        for name, block in server_containers(doc):
            ref = jwt_ref(block)
            where = f"{src} container {name}"
            if ref is None:
                errors.append(f"{where}: no JWT_SECRET env (the binary refuses to start without it)")
                continue
            refs[where] = ref
    if len(set(refs.values())) > 1:
        detail = "; ".join(f"{w} -> {r[0]}/{r[1]}" for w, r in sorted(refs.items()))
        errors.append(f"JWT_SECRET comes from different Secrets: {detail}")
    found = len(refs) + sum(1 for e in errors if "no JWT_SECRET env" in e)
    if expect is not None and found != expect:
        errors.append(f"expected {expect} Phoenix server Deployment(s), rendered {found}")
    if errors:
        for e in errors:
            print("FAIL:", e, file=sys.stderr)
        return 1
    for where, (name, key) in sorted(refs.items()):
        print(f"ok: {where} JWT_SECRET <- secret {name} key {key}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
