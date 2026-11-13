#!/usr/bin/env python3
"""Run every test named in docs/multi-region/M6_MATRIX_MAP.tsv and tally it per §13 T row.

Usage: scripts/m6_matrix_run.py <outdir>

Runs `go test -race -count=1 -json` once per package with an exact top-level
`-run` regex. Set TEST_MARIADB_DSN (an owned, disposable database) to include
the MariaDB leaves of the hub repository contracts; it is passed through and
never printed. Writes one .jsonl per package and summary.json to <outdir>.
Exits 1 if any mapped test fails or does not run at all. A skip is reported
but is not a failure, because fixture-gated rows (T20) skip by design.
"""
import collections
import json
import os
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAP = os.path.join(ROOT, "docs", "multi-region", "M6_MATRIX_MAP.tsv")


def load_rows():
    rows = []
    for line in open(MAP, encoding="utf-8"):
        if line.strip() and not line.startswith("#"):
            t, verdict, pkg, tests, note = (line.rstrip("\n").split("\t") + [""])[:5]
            rows.append(dict(T=t, verdict=verdict, pkg=pkg, tests=tests.split("|"), note=note))
    return rows


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    rows = load_rows()
    bypkg = collections.defaultdict(set)
    for r in rows:
        bypkg[r["pkg"]].update(r["tests"])
    leaves = collections.defaultdict(dict)  # (pkg, top) -> {leaf: action}
    for pkg, names in sorted(bypkg.items()):
        log = os.path.join(out, pkg.strip("./").replace("/", "_") + ".jsonl")
        start = time.time()
        with open(log, "w", encoding="utf-8") as f:
            code = subprocess.run(
                ["go", "test", "-race", "-count=1", "-timeout", "40m", "-json",
                 "-run", "^(" + "|".join(sorted(names)) + ")$", pkg],
                cwd=ROOT, stdout=f, stderr=subprocess.STDOUT, check=False).returncode
        print(f"{pkg}: exit {code} in {time.time() - start:.0f}s", flush=True)
        for line in open(log, encoding="utf-8"):
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if e.get("Test") and e.get("Action") in ("pass", "fail", "skip"):
                leaves[(pkg, e["Test"].split("/")[0])][e["Test"]] = e["Action"]
    summary, unique = [], {}
    for r in rows:
        for name in r["tests"]:
            got = leaves.get((r["pkg"], name), {})
            c = collections.Counter(got.values())
            maria = collections.Counter(a for k, a in got.items() if "mariadb" in k.lower())
            entry = dict(T=r["T"], verdict=r["verdict"], pkg=r["pkg"], test=name,
                         top=got.get(name, "NOT-RUN"), leaves_pass=c["pass"],
                         leaves_fail=c["fail"], leaves_skip=c["skip"],
                         mariadb_pass=maria["pass"], mariadb_skip=maria["skip"],
                         mariadb_fail=maria["fail"])
            summary.append(entry)
            unique[(r["pkg"], name)] = entry
    with open(os.path.join(out, "summary.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=1)
    u = list(unique.values())
    print("T rows:", len({r["T"] for r in rows}), "| unique tests:", len(u),
          dict(collections.Counter(e["top"] for e in u)))
    print("leaves: pass", sum(e["leaves_pass"] for e in u), "fail", sum(e["leaves_fail"] for e in u),
          "skip", sum(e["leaves_skip"] for e in u), "| mariadb leaves: pass",
          sum(e["mariadb_pass"] for e in u), "skip", sum(e["mariadb_skip"] for e in u),
          "fail", sum(e["mariadb_fail"] for e in u))
    bad = False
    for e in u:
        if e["top"] in ("fail", "NOT-RUN") or e["leaves_fail"]:
            bad = True
            print("FAIL", e["T"], e["pkg"], e["test"], e["top"])
        elif e["top"] == "skip" or e["leaves_skip"]:
            print("SKIP", e["T"], e["pkg"], e["test"])
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
