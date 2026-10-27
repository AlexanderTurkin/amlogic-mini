#!/usr/bin/env python3
"""Build native packages from pinned USB sources. Python is build-time only."""

import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import shlex
import subprocess
import tarfile
import tempfile
import urllib.request
import zipfile

import windows_driver
import windows_input
import licensing

ROOT = Path(__file__).resolve().parents[1]
VERSION = (os.environ.get("AMLOGIC_MINI_VERSION") or "0.4.0-rc12").removeprefix("v")
USB_VERSION = "1.0.30"
ASSETS = {
    "libusb-1.0.30.tar.bz2": "fea36f34f9156400209595e300840767ab1a385ede1dc7ee893015aea9c6dbaf",
}
DEFAULT_TARGETS = ("darwin-arm64", "windows-amd64", "linux-amd64")
TARGETS = (
    "darwin-arm64",
    "darwin-amd64",
    "linux-arm64",
    "linux-amd64",
    "windows-amd64",
    "windows-arm64",
)


def run(args, *, cwd=ROOT, env=None, log=None):
    subprocess.run(
        [str(x) for x in args],
        cwd=cwd,
        env=env,
        check=True,
        stdout=log,
        stderr=subprocess.STDOUT if log else None,
    )


def prepare(cache, work):
    cache.mkdir(parents=True, exist_ok=True)
    for name, want in ASSETS.items():
        bundled = ROOT / "third_party" / name
        path = bundled if bundled.is_file() else cache / name
        if not path.exists():
            with urllib.request.urlopen(
                f"https://github.com/libusb/libusb/releases/download/v{USB_VERSION}/{name}",
                timeout=60,
            ) as response:
                path.write_bytes(response.read())
        if hashlib.sha256(path.read_bytes()).hexdigest() != want:
            raise RuntimeError(f"USB source checksum mismatch: {name}")
    source = work / f"libusb-{USB_VERSION}"
    if not source.exists():
        with tarfile.open(path) as archive:
            archive.extractall(work, filter="data")
    return source


def windows_usb(source, builddir, prefix, env, log):
    config_env = env.copy()
    config_env.update(AR="zig ar", RANLIB="zig ranlib", CFLAGS="-Os")
    cpu = "aarch64" if env["GOARCH"] == "arm64" else "x86_64"
    run(
        [
            source / "configure",
            f"--prefix={prefix}",
            f"--host={cpu}-w64-mingw32",
            "--disable-shared",
            "--enable-static",
            "--disable-examples-build",
            "--disable-tests-build",
        ],
        cwd=builddir,
        env=config_env,
        log=log,
    )
    # Upstream Windows sources; only the resource-only version metadata is omitted.
    names = [
        "core.c",
        "descriptor.c",
        "hotplug.c",
        "io.c",
        "strerror.c",
        "sync.c",
        "os/events_windows.c",
        "os/threads_windows.c",
        "os/windows_common.c",
        "os/windows_usbdk.c",
        "os/windows_winusb.c",
    ]
    objects = []
    for name in names:
        obj = builddir / Path(name).with_suffix(".o").name
        run(
            [
                *shlex.split(env["CC"]),
                "-Os",
                "-U_WIN32_WINNT",
                "-DHAVE_CONFIG_H",
                f"-I{builddir}",
                f"-I{source}/libusb",
                "-c",
                source / "libusb" / name,
                "-o",
                obj,
            ],
            env=env,
            log=log,
        )
        objects.append(obj)
    run(["zig", "ar", "rcs", prefix / "lib/libusb-1.0.a", *objects], log=log)
    shutil.copy2(source / "libusb/libusb.h", prefix / "include/libusb-1.0/libusb.h")


def build(target, source, work, dist):
    with tempfile.TemporaryDirectory(prefix=".amlogic-mini-stage-", dir=dist) as stage:
        return build_staged(target, source, work, dist, Path(stage))


def build_staged(target, source, work, dist, stage):
    system, arch = target.split("-")
    prefix = work / target / "prefix"
    if system == "windows":
        prefix = work / target / "native-prefix"
    builddir = work / target / "build"
    builddir.mkdir(parents=True, exist_ok=True)
    (prefix / "lib/pkgconfig").mkdir(parents=True, exist_ok=True)
    (prefix / "include/libusb-1.0").mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.update(
        GOOS=system,
        GOARCH=arch,
        CGO_ENABLED="1",
        GOCACHE=env.get("GOCACHE", "/private/tmp/amlogic-mini-go-cache"),
        GOMODCACHE=env.get("GOMODCACHE", "/private/tmp/amlogic-mini-go-mod"),
    )
    extra = ""
    if system == "darwin":
        env["CC"] = f"clang -arch {'arm64' if arch == 'arm64' else 'x86_64'}"
        host = f"{'aarch64' if arch == 'arm64' else 'x86_64'}-apple-darwin"
        extra = "-lobjc -framework IOKit -framework CoreFoundation -framework Security"
    elif system == "linux":
        cpu = "aarch64" if arch == "arm64" else "x86_64"
        env["CC"] = f"zig cc -target {cpu}-linux-musl"
        env["AR"] = "zig ar"
        env["RANLIB"] = "zig ranlib"
        if arch == "amd64":
            env["GOAMD64"] = "v1"
        host = f"{cpu}-linux-musl"
        extra = "-lpthread"
    else:
        cpu = "aarch64" if arch == "arm64" else "x86_64"
        env["CC"] = f"zig cc -target {cpu}-windows-gnu"
        extra = "-luser32 -ladvapi32 -lole32 -lsetupapi -lcfgmgr32"
        if not (prefix / "lib/libusb-1.0.a").exists():
            with (dist / f"{target}-libusb-build.log").open("w") as log:
                windows_usb(source, builddir, prefix, env, log)
    if system != "windows" and not (prefix / "lib/libusb-1.0.a").exists():
        config_env = env.copy()
        config_env["CFLAGS"] = "-Os"
        with (dist / f"{target}-libusb-build.log").open("w") as log:
            run(
                [
                    source / "configure",
                    f"--prefix={prefix}",
                    f"--host={host}",
                    "--disable-shared",
                    "--enable-static",
                    "--disable-udev",
                    "--disable-examples-build",
                    "--disable-tests-build",
                ],
                cwd=builddir,
                env=config_env,
                log=log,
            )
            run(["make", "-j4"], cwd=builddir, env=config_env, log=log)
            run(["make", "install"], cwd=builddir, env=config_env, log=log)
    pc = prefix / "lib/pkgconfig/libusb-1.0.pc"
    pc.write_text(
        f"Name: libusb\nDescription: pinned USB runtime\nVersion: {USB_VERSION}\nCflags: -I{prefix}/include/libusb-1.0\nLibs: {prefix}/lib/libusb-1.0.a {extra}\n"
    )
    env["PKG_CONFIG_PATH"] = str(pc.parent)
    driver = None
    if system == "windows":
        with (dist / f"{target}-driver-build.log").open("w") as log:
            driver = windows_driver.build(work, arch, env, log)
        env["PKG_CONFIG_PATH"] += os.pathsep + str(driver / "lib/pkgconfig")
    # Go does not track external C archive contents in its build cache. Make
    # the selected USB archive part of the C compilation key as well.
    usb_hash = hashlib.sha256((prefix / "lib/libusb-1.0.a").read_bytes()).hexdigest()[
        :16
    ]
    env["CGO_CFLAGS"] = f"-O2 -DAMLOGIC_MINI_LIBUSB_BUILD_ID=0x{usb_hash}"
    if driver is not None:
        digest = hashlib.sha256((driver / "lib/libwdi.a").read_bytes()).hexdigest()[:16]
        env["CGO_CFLAGS"] += f" -DAMLOGIC_MINI_WDI_BUILD_ID=0x{digest}"
    package = stage / f"amlogic-mini-{VERSION}-{target}"
    package.mkdir()
    for name in ("backups", "alpine"):
        (package / name).mkdir()
    executable = package / (
        "amlogic-mini.exe" if system == "windows" else "amlogic-mini"
    )
    ldflags = f"-s -w -X amlogicmini/internal/cli.Version={VERSION} -linkmode external"
    if system == "linux":
        ldflags += " -extldflags '-static -s'"
    go_root = windows_input.prepare(work / target, env) if system == "windows" else ROOT
    with (dist / f"{target}-go-build.log").open("w") as log:
        run(
            [
                "go",
                "build",
                *(["-tags", "amlogic_mini_driver"] if system == "windows" else []),
                *(["-mod=vendor"] if system == "windows" else []),
                "-trimpath",
                "-buildvcs=false",
                "-ldflags",
                ldflags,
                "-o",
                executable,
                "./cmd/amlogic-mini",
            ],
            cwd=go_root,
            env=env,
            log=log,
        )
    if system == "windows":
        (package / "libusb-1.0.dll").unlink(missing_ok=True)
    if system == "darwin":
        run(["codesign", "--force", "--sign", "-", executable])
    verify_notices(executable, env, source, driver)
    archive = stage / (package.name + ".zip")
    with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as out:
        for path in sorted(package.rglob("*")):
            out.write(path, path.relative_to(package))
    archive = Path(shutil.move(archive, dist / archive.name))
    return {
        "target": target,
        "built": True,
        "archive": archive.name,
        "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
        "usb_hardware_tested": False,
    }


def verify_notices(executable, env, source, driver):
    def normalized(text):
        return "\n".join(line.rstrip() for line in text.splitlines()).strip()

    notices = normalized((ROOT / "internal/appinfo/notices.txt").read_text())

    def require(path, label):
        if not path.is_file() or normalized(path.read_text()) not in notices:
            raise RuntimeError(f"missing embedded license: {label}")

    archives = dict(ASSETS)
    if driver is not None:
        archives.update(
            {name: digest for name, (_, digest) in windows_driver.ASSETS.items()}
        )
    for name, digest in archives.items():
        path = ROOT / "third_party" / name
        if (
            not path.is_file()
            or hashlib.sha256(path.read_bytes()).hexdigest() != digest
        ):
            raise RuntimeError(f"missing or incorrect corresponding source: {name}")
    require(ROOT / "LICENSE", "application MIT")
    require(ROOT / "third_party/Unicode-LICENSE.txt", "Unicode data")
    require(source / "COPYING", "libusb")
    require(source / "AUTHORS", "libusb attribution")
    if driver is not None:
        for name in ("libwdi-LGPL-3.txt", "libwdi-GPL-3.txt", "libwdi-AUTHORS.txt"):
            require(driver / "licenses" / name, "libwdi")
    if env["GOOS"] in ("linux", "windows"):
        for label, text in licensing.zig_runtime_notices(env["GOOS"]):
            if normalized(text) not in notices:
                raise RuntimeError(f"missing embedded runtime notice: {label}")
    goroot = Path(
        subprocess.check_output(["go", "env", "GOROOT"], env=env, text=True).strip()
    )
    go_license = goroot / "LICENSE"
    # Homebrew moves Go's top-level documentation beside libexec.
    if not go_license.is_file() and goroot.name == "libexec":
        go_license = goroot.parent / "LICENSE"
    require(go_license, "Go")
    metadata = subprocess.check_output(
        ["go", "version", "-m", str(executable)], env=env, text=True
    )
    dependencies = []
    for line in metadata.splitlines():
        fields = line.split()
        if fields and fields[0] == "=>":
            raise RuntimeError("release dependencies must not use module replacements")
        if fields and fields[0] == "dep":
            dependencies.append((fields[1], fields[2]))
    if not dependencies:
        raise RuntimeError("binary contains no dependency metadata")
    for module, version in dependencies:
        escaped = "".join("!" + c.lower() if c.isupper() else c for c in module)
        directory = Path(env["GOMODCACHE"]) / f"{escaped}@{version}"
        candidates = [directory / name for name in licensing.LEGAL_FILES]
        found = [path for path in candidates if path.is_file()]
        if not found:
            raise RuntimeError(f"missing license for linked module {module}@{version}")
        if f"=== {module}@{version}/" not in notices:
            raise RuntimeError(f"missing embedded license version: {module}@{version}")
        for path in found:
            require(path, f"{module}@{version}")


def source_artifacts(dist):
    # Export only public source files. Never export Git metadata or user data.
    names = {
        "go.mod",
        "go.sum",
        "README.md",
        "LICENSE",
        ".gitignore",
        ".github/workflows/ci.yml",
        "internal/appinfo/notices.txt",
        "third_party/Unicode-LICENSE.txt",
    }
    for pattern in (
        "cmd/amlogic-mini/*.go",
        "internal/*/*.go",
        "scripts/*.py",
        "scripts/*.patch",
        "third_party/*.tar.*",
    ):
        names.update(str(path.relative_to(ROOT)) for path in ROOT.glob(pattern))
    archive = dist / "amlogic-mini-source.zip"
    temporary = archive.with_suffix(".zip.partial")
    with zipfile.ZipFile(temporary, "w", zipfile.ZIP_DEFLATED) as out:
        for name in sorted(names):
            path = ROOT / name
            if not path.exists():
                continue
            if any(
                part.is_symlink()
                for part in (path, *path.parents)
                if part != ROOT and part.is_relative_to(ROOT)
            ):
                raise RuntimeError(f"source symlink is forbidden: {name}")
            if not path.is_file() or not path.resolve().is_relative_to(ROOT.resolve()):
                raise RuntimeError(f"source is not a regular public file: {name}")
            out.write(path, "amlogic-mini/" + name)
    temporary.replace(archive)
    return archive


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", action="append", choices=TARGETS)
    parser.add_argument("--work", default="/private/tmp/amlogic-mini-release-1.0.30")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9A-Za-z][0-9A-Za-z.+-]{0,63}", VERSION):
        parser.error("invalid release version")
    dist = ROOT / "dist"
    dist.mkdir(exist_ok=True)
    work = Path(args.work)
    work.mkdir(parents=True, exist_ok=True)
    source = prepare(ROOT / ".cache/deps", work)
    if any(target.startswith("windows") for target in args.target or DEFAULT_TARGETS):
        windows_driver.prepare(ROOT / ".cache/deps", work)
    report = []
    for target in args.target or DEFAULT_TARGETS:
        print(f"Building {target}", flush=True)
        try:
            report.append(build(target, source, work, dist))
            print(f"Built {target}", flush=True)
        except Exception as error:
            report.append({"target": target, "built": False, "error": str(error)})
            print(f"FAILED {target}: {error}", flush=True)
        (dist / "BUILD-REPORT.json").write_text(json.dumps(report, indent=2) + "\n")
    if not all(item["built"] for item in report):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
