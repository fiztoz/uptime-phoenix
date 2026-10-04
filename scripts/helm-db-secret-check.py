#!/usr/bin/env python3
"""Check the rendered in-release MariaDB DSN without printing credentials.

Consumes Helm output on stdin. The anchored field matches intentionally target
this chart's rendered Secret/ConfigMap shape; no YAML dependency is required.
"""

import base64
import binascii
import re
import sys


def check(rendered):
    documents = re.split(r"^---\s*$", rendered, flags=re.MULTILINE)
    secrets = [doc for doc in documents if re.search(r"^kind: Secret$", doc, re.MULTILINE)
               and re.search(r"^  name: uptime-phoenix$", doc, re.MULTILINE)]
    if len(secrets) != 1:
        raise ValueError("expected exactly one Phoenix Secret")
    values = re.findall(r'^  db-dsn: "([A-Za-z0-9+/=]*)"$', secrets[0], re.MULTILINE)
    if len(values) != 1:
        raise ValueError("expected one encoded db-dsn in the Phoenix Secret")
    try:
        dsn = base64.b64decode(values[0], validate=True).decode("utf-8")
    except (binascii.Error, UnicodeError) as error:
        raise ValueError("db-dsn is not valid encoded text") from error
    if "@tcp(uptime-phoenix-mariadb:3306)/phoenix?" not in dsn:
        raise ValueError("Secret DSN does not target the in-release MariaDB Service")
    for doc in documents:
        if re.search(r"^kind: ConfigMap$", doc, re.MULTILINE) and re.search(r"^  db-dsn:", doc, re.MULTILINE):
            raise ValueError("database credentials must not be rendered in a ConfigMap")
    reference = r"name: DB_DSN\s+valueFrom:\s+secretKeyRef:\s+name: uptime-phoenix\s+key: db-dsn"
    if not re.search(reference, rendered):
        raise ValueError("application DB_DSN must reference the Phoenix Secret")


if __name__ == "__main__":
    try:
        check(sys.stdin.read())
    except ValueError as error:
        print(f"helm-db-secret-check: {error}", file=sys.stderr)
        raise SystemExit(1)
    print("helm-db-secret-check: Secret DSN and workload reference verified")
