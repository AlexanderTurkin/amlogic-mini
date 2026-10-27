"""The Windows input workaround must own each delivered byte slice."""

import hashlib
from pathlib import Path
import subprocess
import shutil
import tempfile
import unittest

import windows_input


class InputPatchTests(unittest.TestCase):
    def test_input_chunks_survive_buffer_reuse(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "main.go"
            original = b"""package main
import "bytes"
func main() {
    readc := make(chan []byte)
    done := make(chan struct{})
    go func() {
        var buf bytes.Buffer
        buf.WriteString("first")
        select {
        case readc <- buf.Bytes():
        }
        buf.Reset()
        buf.WriteString("other")
        close(done)
    }()
    first := <-readc
    <-done
    if string(first) != "first" { panic("input buffer was reused") }
}
"""
            source.write_bytes(original)
            broken = subprocess.run(["go", "run", str(source)], capture_output=True)
            self.assertNotEqual(broken.returncode, 0)
            self.assertIn(b"input buffer was reused", broken.stderr)
            patched = root / "patched input with spaces.go"
            shutil.copyfile(source, patched)
            windows_input.patch_input(patched, hashlib.sha256(original).hexdigest())
            fixed = subprocess.run(["go", "run", str(patched)], capture_output=True)
            self.assertEqual(fixed.returncode, 0, fixed.stderr.decode())
            self.assertEqual(source.read_bytes(), original)

    def test_changed_dependency_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "input.go"
            source.write_bytes(b"unexpected source")
            with self.assertRaisesRegex(RuntimeError, "checksum"):
                windows_input.patch_input(source, "0" * 64)
            self.assertEqual(source.read_bytes(), b"unexpected source")
