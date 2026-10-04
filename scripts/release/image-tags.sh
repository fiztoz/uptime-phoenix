#!/usr/bin/env bash
# Print the Docker tags for one image/version, one tag per line.
set -euo pipefail

if [[ $# -ne 2 || -z "$1" || ! "$2" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  echo "Usage: $0 <image> <semver-version>" >&2
  exit 2
fi

printf '%s:%s\n' "$1" "$2"
if [[ "$2" != *-* ]]; then
  printf '%s:latest\n' "$1"
fi
