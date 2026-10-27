"""Build Windows with a private vendor copy and fixed console input ownership.

Ultraviolet reuses a buffer after sending its bytes to another goroutine.
Patch the pinned file in a temporary vendor tree; never modify the module cache.
"""

import argparse
import hashlib
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/charmbracelet/ultraviolet"
SOURCE_SHA256 = "4022cce38469b8d610bcb9ee0e75c3b9d1721b30e3de67ce9bd8d2c9b1b61358"


def patch_input(source, expected_digest=SOURCE_SHA256):
    data = source.read_bytes()
    if hashlib.sha256(data).hexdigest() != expected_digest:
        raise RuntimeError("Windows input source checksum mismatch")
    old = b"case readc <- buf.Bytes():"
    if data.count(old) != 1:
        raise RuntimeError("unexpected Windows input reader")
    source.write_bytes(data.replace(old, b"case readc <- bytes.Clone(buf.Bytes()):"))


def prepare(work, env=None):
    work.mkdir(parents=True, exist_ok=True)
    build = Path(tempfile.mkdtemp(prefix="windows-go-", dir=work)).resolve()
    names = {"go.mod", "go.sum", "internal/appinfo/notices.txt"}
    for pattern in ("cmd/**/*.go", "internal/**/*.go"):
        names.update(str(p.relative_to(ROOT)) for p in ROOT.glob(pattern))
    for name in sorted(names):
        source = ROOT / name
        if any(p.is_symlink() for p in (source, *source.parents) if p != ROOT):
            raise RuntimeError(f"source symlink is forbidden: {name}")
        target = build / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
    subprocess.run(["go", "mod", "vendor"], cwd=build, env=env, check=True)
    patch_input(build / "vendor" / MODULE / "terminal_reader_windows.go")
    return build


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", type=Path, required=True)
    args = parser.parse_args()
    sys.stdout.buffer.write((prepare(args.work).as_posix() + "\n").encode("utf-8"))
