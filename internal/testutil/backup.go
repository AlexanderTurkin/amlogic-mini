package testutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
)

// StockBackup uses full-sized sparse files to exercise the production layout.
func StockBackup(t testing.TB, partial bool) string {
	t.Helper()
	dir := t.TempDir()
	p := profile.A113X()
	m := &archive.Manifest{Schema: archive.SchemaVersion, Version: "test", Profile: p, Mode: "partitions", Status: "complete", Firmware: [7]byte{0, 7, 0, 16}, Started: "2026-10-27T00:00:00Z"}
	for name, size := range archive.ExpectedFiles(p, "partitions") {
		expected := size
		status := "verified"
		if size == 0 {
			size = 1
			expected = 1
		}
		if name == "data.bin" && partial {
			size -= 262144
			status = "partial"
			m.Status = "warnings"
		}
		if name == "env.txt" {
			size = int64(len("bootcmd=run storeboot\n"))
			expected = size
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if name == "boot_a.bin" || name == "boot_b.bin" {
			if _, err := f.WriteAt([]byte("ANDROID!"), 0); err != nil {
				t.Fatal(err)
			}
		}
		if name == "env.txt" {
			if _, err := f.WriteAt([]byte("bootcmd=run storeboot\n"), 0); err != nil {
				t.Fatal(err)
			}
		}
		f.Close()
		hash, _, err := archive.HashFile(context.Background(), filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, archive.File{Name: name, Size: size, Expected: expected, SHA256: hash, Status: status, DoubleRead: true})
		if name == "rsv-keys.bin" {
			m.Identity = hash
		}
	}
	if err := archive.Save(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}
