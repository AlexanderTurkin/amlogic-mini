"""Build libwdi for Windows 10+ inbox WinUSB without Microsoft redistributables."""

import hashlib
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request

VERSION = "1.5.1"
ASSETS = {
    "libwdi-1.5.1.tar.gz": (
        "https://codeload.github.com/pbatard/libwdi/tar.gz/refs/tags/v1.5.1",
        "a695e93db0977dfdc5c6a99a4ea91b22f9027547d0177b2a0f3075078643c929",
    ),
}


def prepare(cache, work):
    for name, (url, digest) in ASSETS.items():
        bundled = Path(__file__).resolve().parents[1] / "third_party" / name
        path = bundled if bundled.is_file() else cache / name
        if not path.exists():
            with urllib.request.urlopen(url, timeout=60) as response:
                path.write_bytes(response.read())
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise RuntimeError(f"driver input checksum mismatch: {name}")
        source = work / f"libwdi-{VERSION}"
        if not source.exists():
            with tarfile.open(path) as archive:
                archive.extractall(work, filter="data")
    return {name: digest for name, (_, digest) in ASSETS.items()}


def inbox_source(work):
    source = work / f"libwdi-{VERSION}-inbox"
    shutil.copytree(work / f"libwdi-{VERSION}", source, dirs_exist_ok=True)
    # Upstream INF/CAT templates use CRLF; normalize only the build copy so
    # the published text patch applies consistently with BSD and GNU patch.
    for name in ("winusb.inf.in", "winusb.cat.in"):
        path = source / "libwdi" / name
        path.write_text(path.read_text())
    subprocess.run(
        [
            "patch",
            "--batch",
            "--forward",
            "-p1",
            "-i",
            str(Path(__file__).resolve().with_name("libwdi-inbox-winusb.patch")),
        ],
        cwd=source,
        check=True,
        stdout=subprocess.DEVNULL,
    )
    return source


def build(work, arch, env, log):
    source = inbox_source(work)
    upstream = source / "libwdi"
    builddir = work / f"libwdi-{VERSION}-{arch}"
    prefix = builddir / "prefix"
    (prefix / "lib/pkgconfig").mkdir(parents=True, exist_ok=True)
    (prefix / "include").mkdir(exist_ok=True)
    (prefix / "licenses").mkdir(exist_ok=True)

    def run(args, cwd=builddir):
        subprocess.run(
            [str(a) for a in args],
            cwd=cwd,
            env=env,
            check=True,
            stdout=log,
            stderr=subprocess.STDOUT,
        )

    config = (
        "#define OPT_M64 1\n"
        + ("#define OPT_ARM 1\n" if arch == "arm64" else "")
        + '#define WDK_DIR "inbox"\n'
        + "#define WDF_VER 1011\n"
        + "#define ENABLE_LOGGING 1\n#define INCLUDE_DEBUG_LOGGING 1\n"
    )
    (builddir / "config.h").write_text(config)
    includes = [f"-I{builddir}", f"-I{upstream}"]
    flags = [
        "-Os",
        "-U_WIN32_WINNT",
        "-DWINVER=0x0601",
        "-D_WIN32_WINNT=0x0601",
        "-D_WIN32_IE=0x0700",
        "-DCOBJMACROS",
        "-Wno-pointer-sign",
        *includes,
    ]
    architectures = [("x86_64", "x64")]
    if arch == "arm64":
        architectures.append(("aarch64", "arm64"))
    for cpu, suffix in architectures:
        run(
            [
                "zig",
                "cc",
                "-target",
                cpu + "-windows-gnu",
                *flags,
                upstream / "installer.c",
                "-lsetupapi",
                "-lnewdev",
                "-lole32",
                "-ladvapi32",
                "-o",
                builddir / f"installer_{suffix}.exe",
            ]
        )
    run(
        [
            "clang",
            *includes,
            f'-DSOLUTIONDIR="{builddir}"',
            upstream / "embedder.c",
            "-o",
            builddir / "embedder",
        ]
    )
    run([builddir / "embedder", builddir / "embedded.h"], cwd=upstream)
    cpu = "aarch64" if arch == "arm64" else "x86_64"
    objects = []
    for name in ("logging", "tokenizer", "vid_data", "pki", "libwdi_dlg", "libwdi"):
        obj = builddir / (name + ".o")
        run(
            [
                "zig",
                "cc",
                "-target",
                cpu + "-windows-gnu",
                *flags,
                "-c",
                upstream / (name + ".c"),
                "-o",
                obj,
            ]
        )
        objects.append(obj)
    run(["zig", "ar", "rcs", prefix / "lib/libwdi.a", *objects])
    shutil.copy2(upstream / "libwdi.h", prefix / "include/libwdi.h")
    for original, name in (
        (source / "COPYING", "libwdi-GPL-3.txt"),
        (source / "COPYING-LGPL", "libwdi-LGPL-3.txt"),
        (source / "AUTHORS", "libwdi-AUTHORS.txt"),
    ):
        destination = prefix / "licenses" / name
        destination.unlink(missing_ok=True)
        shutil.copy2(original, destination)
    (prefix / "lib/pkgconfig/libwdi.pc").write_text(
        f"Name: libwdi\nDescription: embedded WinUSB installer\nVersion: {VERSION}\n"
        f"Cflags: -I{prefix}/include\n"
        f"Libs: {prefix}/lib/libwdi.a -lsetupapi -lnewdev -lole32 -lntdll -ladvapi32 -lshell32 -lcomctl32 -luser32\n"
    )
    return prefix
