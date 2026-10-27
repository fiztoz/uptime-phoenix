#!/usr/bin/env python3
"""Fail unless FILE is an ELF64 executable whose e_machine matches linux/ARCH.

Usage: check-elf-arch.py FILE ARCH   (ARCH: amd64 | arm64)

Used by the release dry-run, CI, and the post-publish image check so an arm64
image can never ship an x86-64 binary again (v0.5.1 / v0.6.0 regression).
"""
import pathlib
import struct
import sys

EXPECTED = {"amd64": 62, "arm64": 183}  # EM_X86_64, EM_AARCH64


def main(argv):
    if len(argv) != 3:
        raise SystemExit("usage: check-elf-arch.py FILE ARCH")
    path, arch = pathlib.Path(argv[1]), argv[2]
    header = path.read_bytes()[:20]
    if len(header) < 20 or header[:4] != b"\x7fELF":
        raise SystemExit(f"{path}: extracted file is not an ELF executable")
    if header[4] != 2 or header[5] not in (1, 2):
        raise SystemExit(f"{path}: expected an ELF64 binary")
    endian = "<" if header[5] == 1 else ">"
    machine = struct.unpack(endian + "H", header[18:20])[0]
    expected = EXPECTED.get(arch)
    if expected is None or machine != expected:
        raise SystemExit(f"{path}: ELF machine {machine} does not match linux/{arch}")
    print(f"{path}: ELF64 machine={machine} linux/{arch}")


if __name__ == "__main__":
    main(sys.argv)
