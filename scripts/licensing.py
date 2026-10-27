"""License inputs for the statically linked release components."""

from pathlib import Path
import re
import shutil

LEGAL_FILES = (
    "LICENSE",
    "LICENSE.txt",
    "LICENSE.md",
    "COPYING",
    "COPYRIGHT",
    "NOTICE",
    "NOTICE.txt",
    "NOTICE.md",
    "AUTHORS",
    "PATENTS",
)


def zig_runtime_notices(system="windows"):
    binary = Path(shutil.which("zig")).resolve()
    candidates = (binary.parent / "lib", binary.parent.parent / "lib/zig")
    lib = next((p for p in candidates if (p / "libc/mingw/COPYING").is_file()), None)
    if lib is None:
        raise RuntimeError("cannot locate Zig runtime licenses")
    license_path = next(
        (
            p
            for p in (binary.parent / "LICENSE", binary.parent.parent / "LICENSE")
            if p.is_file()
        ),
        None,
    )
    if license_path is None:
        raise RuntimeError("cannot locate Zig license")
    parts = [
        ("Zig runtime", license_path.read_text()),
        ("musl runtime", (lib / "libc/musl/COPYRIGHT").read_text()),
    ]
    if system == "linux":
        return parts
    parts.append(("MinGW-w64", (lib / "libc/mingw/COPYING").read_text()))
    seen = set()
    for path in sorted((lib / "libc/mingw").rglob("*")):
        if path.suffix.lower() not in (".c", ".h", ".s"):
            continue
        for block in re.findall(r"/\*.*?\*/", path.read_text(errors="replace"), re.S):
            if not re.search(r"copyright|permission|public domain", block, re.I):
                continue
            if block not in seen:
                parts.append(
                    ("MinGW-w64/" + str(path.relative_to(lib / "libc/mingw")), block)
                )
                seen.add(block)
    return parts
