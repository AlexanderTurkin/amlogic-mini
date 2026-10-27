"""Release regressions use synthetic files and a compiler stub, never backups."""

import hashlib
import io
from pathlib import Path
import sys
import tempfile
import tarfile
import unittest
from unittest import mock
import zipfile

import release


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dist = self.root / "dist"
        self.dist.mkdir()
        self.work = self.root / "work"
        self.modules = self.root / "modules"
        self.source = self.work / f"libusb-{release.USB_VERSION}"
        self.source.mkdir(parents=True)
        (self.source / "COPYING").write_text("USB license")
        (self.source / "AUTHORS").write_text("USB authors")
        for name, data in {
            "README.md": "public readme",
            "go.mod": "module amlogicmini\n\ngo 1.26.0\n",
            "go.sum": "",
            "LICENSE": "Application MIT license",
            "third_party/Unicode-LICENSE.txt": "Unicode data permission",
            "third_party/libusb-1.0.30.tar.bz2": "verified USB source archive",
            "third_party/libwdi-1.5.1.tar.gz": "verified driver source archive",
            "docs/private-notes.md": "PRIVATE_DOC",
            "internal/appinfo/notices.txt": "Unicode data permission\nApplication MIT license\nGo license\nUSB license\nUSB authors\n=== example.org/library@v1.2.3/LICENSE ===\nACTUAL_LINKED_MODULE_LICENSE\ndriver license\nbase GPL license\ndriver authors\nMicrosoft WDF redistributables",
            "cmd/amlogic-mini/main.go": "package main\nfunc main() {}\n",
            "internal/demo/example.go": "package demo\n",
            "scripts/release.py": "# public source\n",
            "backups/private/device.bin": "PRIVATE",
            "alpine/Image": "PRIVATE_IMAGE",
            ".env": "PRIVATE_ENV",
        }.items():
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(data)
        self.usb_archive = (
            self.root / ".cache/deps" / f"libusb-{release.USB_VERSION}.tar.bz2"
        )
        self.usb_archive.parent.mkdir(parents=True)
        self.usb_archive.write_bytes(b"verified USB source archive")
        self.homebrew_go = False
        self.build_args = []
        self.build_dirs = []
        self.license = self.modules / "example.org/library@v1.2.3/LICENSE"
        self.license.parent.mkdir(parents=True)
        self.license.write_text("ACTUAL_LINKED_MODULE_LICENSE")
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(
            release.licensing, "zig_runtime_notices", return_value=[]
        ).start()
        mock.patch.object(release, "ROOT", self.root).start()
        mock.patch.object(
            release,
            "ASSETS",
            {
                self.usb_archive.name: hashlib.sha256(
                    self.usb_archive.read_bytes()
                ).hexdigest()
            },
        ).start()
        mock.patch.object(release, "prepare", return_value=self.source).start()
        mock.patch.object(
            release.windows_driver,
            "ASSETS",
            {
                "libwdi-1.5.1.tar.gz": (
                    "unused",
                    hashlib.sha256(b"verified driver source archive").hexdigest(),
                )
            },
        ).start()
        mock.patch.object(release.windows_driver, "prepare", return_value={}).start()
        driver = self.work / "driver-prefix"
        (driver / "lib").mkdir(parents=True)
        (driver / "lib/libwdi.a").write_bytes(b"static installer")
        (driver / "licenses").mkdir()
        for name in ("libwdi-LGPL-3.txt", "Microsoft-WDF-License.rtf"):
            (driver / "licenses" / name).write_text("driver license")
        (driver / "licenses/libwdi-GPL-3.txt").write_text("base GPL license")
        (driver / "licenses/libwdi-AUTHORS.txt").write_text("driver authors")
        mock.patch.object(release.windows_driver, "build", return_value=driver).start()
        mock.patch.object(
            release.windows_input, "prepare", return_value=self.work / "windows-go"
        ).start()
        mock.patch.dict("os.environ", {"GOMODCACHE": str(self.modules)}).start()
        mock.patch.object(release, "run", side_effect=self.compiler).start()
        mock.patch.object(
            release.subprocess, "check_output", side_effect=self.metadata
        ).start()

    def compiler(self, args, **kwargs):
        args = [str(arg) for arg in args]
        if args[:2] == ["go", "build"]:
            self.build_args.append(args)
            self.build_dirs.append(kwargs.get("cwd"))
            Path(args[args.index("-o") + 1]).write_bytes(b"synthetic executable")
        elif args[0] != "codesign":
            raise AssertionError(f"unexpected external command: {args}")

    def metadata(self, args, **kwargs):
        if list(args[:3]) == ["go", "version", "-m"]:
            data = "amlogic-mini:\n\tdep\texample.org/library\tv1.2.3\th1:fixture\n"
        elif list(args) == ["go", "env", "GOROOT"]:
            goroot = self.root / "goroot"
            goroot.mkdir(exist_ok=True)
            (goroot / "LICENSE").write_text("Go license")
            if self.homebrew_go:
                goroot = goroot / "libexec"
                goroot.mkdir(exist_ok=True)
            data = str(goroot) + "\n"
        else:
            raise AssertionError(f"unexpected metadata command: {args}")
        return data if kwargs.get("text") else data.encode()

    def invoke(self, target):
        prefix = (
            self.work
            / target
            / ("native-prefix" if target.startswith("windows") else "prefix")
        )
        (prefix / "lib").mkdir(parents=True, exist_ok=True)
        (prefix / "lib/libusb-1.0.a").write_bytes(b"static USB library")
        with mock.patch.object(
            sys, "argv", ["release.py", "--target", target, "--work", str(self.work)]
        ):
            release.main()

    def test_windows_release_applies_input_ownership_fix(self):
        self.invoke("windows-amd64")
        args = self.build_args[0]
        self.assertIn("-mod=vendor", args)
        self.assertEqual(self.build_dirs[0], self.work / "windows-go")

    def members(self, target):
        path = self.dist / f"amlogic-mini-{release.VERSION}-{target}.zip"
        with zipfile.ZipFile(path) as archive:
            return {
                name: archive.read(name)
                for name in archive.namelist()
                if not name.endswith("/")
            }

    def test_archives_contain_only_executable_and_empty_runtime_folders(self):
        for target, executable in (
            ("darwin-arm64", "amlogic-mini"),
            ("windows-amd64", "amlogic-mini.exe"),
            ("linux-amd64", "amlogic-mini"),
        ):
            with self.subTest(target=target):
                self.invoke(target)
                path = self.dist / f"amlogic-mini-{release.VERSION}-{target}.zip"
                self.assertTrue(
                    path.is_file(), "all user packages must be ZIP archives"
                )
                with zipfile.ZipFile(path) as archive:
                    self.assertEqual(
                        set(archive.namelist()), {executable, "backups/", "alpine/"}
                    )
                    for folder in ("backups/", "alpine/"):
                        self.assertTrue(archive.getinfo(folder).is_dir())

    def test_unsafe_version_is_rejected_before_build(self):
        with mock.patch.object(release, "VERSION", "../private"):
            with self.assertRaises(SystemExit):
                self.invoke("windows-amd64")
        self.assertEqual(self.build_args, [])

    def test_windows_package_enables_embedded_driver(self):
        self.invoke("windows-amd64")
        self.assertIn("amlogic_mini_driver", self.build_args[-1])

    def test_private_files_never_enter_zip_or_tar(self):
        for target in ["windows-amd64", "linux-amd64"]:
            with self.subTest(target=target):
                old = (
                    self.dist
                    / f"amlogic-mini-{release.VERSION}-{target}"
                    / "backups/private"
                )
                old.mkdir(parents=True)
                marker = old / "rsv-keys.bin"
                marker.write_text("PRIVATE_KEY_MARKER")
                self.invoke(target)
                members = self.members(target)
                self.assertFalse(any("backups/" in name for name in members))
                self.assertFalse(
                    any(b"PRIVATE_KEY_MARKER" in data for data in members.values())
                )
                self.assertEqual(marker.read_text(), "PRIVATE_KEY_MARKER")

    def test_source_export_excludes_docs_history_and_user_files(self):
        release.source_artifacts(self.dist)
        with zipfile.ZipFile(self.dist / "amlogic-mini-source.zip") as archive:
            names = archive.namelist()
            self.assertIn("amlogic-mini/go.mod", names)
            self.assertIn("amlogic-mini/cmd/amlogic-mini/main.go", names)
            self.assertIn("amlogic-mini/internal/appinfo/notices.txt", names)
            self.assertFalse(
                any(
                    "backups/" in n
                    or "alpine/" in n
                    or "docs/" in n
                    or ".git/" in n
                    or ".env" in n
                    for n in names
                )
            )

    def test_source_parent_symlink_is_rejected(self):
        private = self.root / "backups/private"
        (private / "secret.go").write_text("PRIVATE_GO_SOURCE")
        (self.root / "internal/linked").symlink_to(private, target_is_directory=True)
        with self.assertRaises(RuntimeError):
            release.source_artifacts(self.dist)

    def test_homebrew_go_license_outside_goroot(self):
        self.homebrew_go = True
        self.invoke("linux-amd64")

    def test_missing_dependency_license_fails_the_release(self):
        self.license.unlink()
        with self.assertRaises(SystemExit):
            self.invoke("linux-amd64")
        self.assertFalse(
            (self.dist / f"amlogic-mini-{release.VERSION}-linux-amd64.zip").exists()
        )

    def test_missing_gpl_base_license_fails_windows_release(self):
        notices = self.root / "internal/appinfo/notices.txt"
        notices.write_text(notices.read_text().replace("base GPL license", ""))
        with self.assertRaises(SystemExit):
            self.invoke("windows-amd64")

    def test_missing_usb_attribution_fails_release(self):
        notices = self.root / "internal/appinfo/notices.txt"
        notices.write_text(notices.read_text().replace("USB authors", ""))
        with self.assertRaises(SystemExit):
            self.invoke("darwin-arm64")

    def test_missing_dependency_notice_fails_release(self):
        (self.license.parent / "NOTICE").write_text("Required upstream attribution")
        with self.assertRaises(SystemExit):
            self.invoke("darwin-arm64")

    def test_source_export_contains_corresponding_library_sources(self):
        release.source_artifacts(self.dist)
        with zipfile.ZipFile(self.dist / "amlogic-mini-source.zip") as archive:
            self.assertEqual(
                archive.read("amlogic-mini/third_party/libusb-1.0.30.tar.bz2"),
                b"verified USB source archive",
            )
            self.assertEqual(
                archive.read("amlogic-mini/third_party/libwdi-1.5.1.tar.gz"),
                b"verified driver source archive",
            )

    def test_missing_bundled_source_prevents_binary_release(self):
        (self.root / "third_party/libusb-1.0.30.tar.bz2").unlink()
        with self.assertRaises(SystemExit):
            self.invoke("darwin-arm64")

    def test_missing_runtime_notice_fails_windows_release(self):
        release.licensing.zig_runtime_notices.return_value = [
            ("CRT", "Required runtime permission")
        ]
        with self.assertRaises(SystemExit):
            self.invoke("windows-amd64")

    def test_missing_unicode_data_notice_fails_release(self):
        notices = self.root / "internal/appinfo/notices.txt"
        notices.write_text(notices.read_text().replace("Unicode data permission", ""))
        with self.assertRaises(SystemExit):
            self.invoke("darwin-arm64")

    def test_default_release_produces_three_platform_archives(self):
        for target in ("darwin-arm64", "windows-amd64", "linux-amd64"):
            prefix = (
                self.work
                / target
                / ("native-prefix" if target.startswith("windows") else "prefix")
            )
            (prefix / "lib").mkdir(parents=True)
            (prefix / "lib/libusb-1.0.a").write_bytes(b"static USB library")
        with mock.patch.object(sys, "argv", ["release.py", "--work", str(self.work)]):
            release.main()
        self.assertEqual(
            {p.name for p in self.dist.glob("*.zip")},
            {
                f"amlogic-mini-{release.VERSION}-darwin-arm64.zip",
                f"amlogic-mini-{release.VERSION}-windows-amd64.zip",
                f"amlogic-mini-{release.VERSION}-linux-amd64.zip",
            },
        )

    def test_missing_linux_runtime_notice_prevents_release(self):
        release.licensing.zig_runtime_notices.return_value = [
            ("musl", "Required Linux runtime permission")
        ]
        with self.assertRaises(SystemExit):
            self.invoke("linux-amd64")


class SourcePreparationTests(unittest.TestCase):
    def test_usb_sources_extract_from_bundled_archive_or_cache(self):
        for origin in ("third_party", "cache"):
            with (
                self.subTest(origin=origin),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = Path(directory)
                cache, work = root / "cache", root / "work"
                work.mkdir()
                path = root / origin / "libusb-1.0.30.tar.bz2"
                path.parent.mkdir()
                with tarfile.open(path, "w:bz2") as archive:
                    item = tarfile.TarInfo("libusb-1.0.30/COPYING")
                    item.size = 11
                    archive.addfile(item, io.BytesIO(b"USB license"))
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
                with (
                    mock.patch.object(release, "ROOT", root),
                    mock.patch.object(release, "ASSETS", {path.name: digest}),
                ):
                    source = release.prepare(cache, work)
                self.assertEqual((source / "COPYING").read_text(), "USB license")


class InboxDriverTests(unittest.TestCase):
    def test_resources_use_system_winusb_without_redistributed_dlls(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            with tarfile.open(
                Path(__file__).resolve().parents[1] / "third_party/libwdi-1.5.1.tar.gz"
            ) as archive:
                archive.extractall(work, filter="data")
            source = release.windows_driver.inbox_source(work)
            files = source / "libwdi"
            inf = (files / "winusb.inf.in").read_text()
            cat = (files / "winusb.cat.in").read_text()
            resources = (files / "embedder_files.h").read_text()
            winusb = resources[
                resources.index("// WinUSB") : resources.index("// libusb0")
            ]
            self.assertNotIn(".dll", winusb.lower())
            self.assertNotIn("coinstaller", inf.lower())
            self.assertNotIn(".dll", cat.lower())
            self.assertIn("Needs   = WINUSB.NT", inf)
            self.assertIn("Needs   = WINUSB.NT.Services", inf)
            self.assertIn("Copyright (c) 2010-2023 Pete Batard", inf)
            original = (work / "libwdi-1.5.1/libwdi/winusb.inf.in").read_text()
            self.assertIn("CoInstallers", original)


if __name__ == "__main__":
    unittest.main()
