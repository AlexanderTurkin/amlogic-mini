package archive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"amlogicmini/internal/profile"
)

func fixture(t *testing.T) (string, *Manifest) {
	t.Helper()
	root := t.TempDir()
	data := []byte("fixture data")
	if err := WriteFile(root, "rsv-keys.bin", data); err != nil {
		t.Fatal(err)
	}
	hash, size, err := HashFile(context.Background(), filepath.Join(root, "rsv-keys.bin"))
	if err != nil {
		t.Fatal(err)
	}
	p := profile.Profile{Name: "fixture", Geometry: profile.Geometry{NANDSize: 8, PageSize: 4, OOBSize: 1, ChunkSize: 4}, Reserved: []profile.Region{{Name: "keys", Offset: 0, Size: 12}}}
	// The real profile is larger; this fixture exercises archive state without USB.
	p.Geometry.NANDSize = 16
	m := &Manifest{Schema: SchemaVersion, Profile: p, Identity: hash, Mode: "partitions", Status: "complete", Files: []File{{Name: "rsv-keys.bin", Size: size, Expected: size, SHA256: hash, Status: "verified", DoubleRead: true}}}
	for name, data := range map[string][]byte{"env.raw.bin": make([]byte, 65536), "env.txt": []byte("bootcmd=run storeboot\n")} {
		if err := WriteFile(root, name, data); err != nil {
			t.Fatal(err)
		}
		h, n, err := HashFile(context.Background(), filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, File{Name: name, Size: n, Expected: n, SHA256: h, Status: "verified", DoubleRead: true})
	}
	return root, m
}

func TestVerifyRejectsChangedFile(t *testing.T) {
	root, m := fixture(t)
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rsv-keys.bin"), []byte("tampered!!!!"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), root); err == nil {
		t.Fatal("corrupted file accepted")
	}
}
func TestVerifyReportsPartialWithoutClaimingComplete(t *testing.T) {
	root, m := fixture(t)
	m.Profile.Geometry.NANDSize = 256
	m.Profile.Partitions = []profile.Region{{Name: "data", Offset: 128, Size: 128}}
	if err := WriteFile(root, "data.bin", []byte("short data")); err != nil {
		t.Fatal(err)
	}
	hash, size, err := HashFile(context.Background(), filepath.Join(root, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	m.Files = append(m.Files, File{Name: "data.bin", Size: size, Expected: 128, SHA256: hash, Status: "partial", DoubleRead: true})
	m.Status = "warnings"
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	r, err := Verify(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("partial file silently considered complete")
	}
}
func TestManifestCannotEscapeBackupDirectory(t *testing.T) {
	for _, name := range []string{"../outside.bin", "/tmp/outside.bin", "..\\outside.bin", "nested/file.bin"} {
		t.Run(name, func(t *testing.T) {
			root, m := fixture(t)
			m.Files[0].Name = name
			if err := Save(root, m); err == nil {
				t.Fatal("unsafe filename accepted")
			}
		})
	}
}
func TestVerifyRejectsSymlink(t *testing.T) {
	root, m := fixture(t)
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "rsv-keys.bin")
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.Rename(file, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Verify(context.Background(), root); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestLoadRejectsFutureSchemaAndUnknownFields(t *testing.T) {
	for _, text := range []string{`{"schema":999}`, `{"schema":2,"surprise":true}`} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil {
			t.Fatal("unsupported manifest accepted")
		}
	}
}
func TestSaveReplacesManifestAndWriteRefusesOverwrite(t *testing.T) {
	root, m := fixture(t)
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	m.Status = "warnings"
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root)
	if err != nil || loaded.Status != "warnings" {
		t.Fatalf("replace manifest: %v", err)
	}
	if err := WriteFile(root, "rsv-keys.bin", []byte("overwrite")); err == nil {
		t.Fatal("existing file overwritten")
	}
}
func TestVerifyCanceledAndIncompleteArchive(t *testing.T) {
	root, m := fixture(t)
	m.Status = "running"
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), root); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("unfinished archive accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := HashFile(ctx, filepath.Join(root, "rsv-keys.bin")); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestNamedHardwareProfileCannotOmitRequiredPartitions(t *testing.T) {
	root, m := fixture(t)
	m.Profile = profile.A113X()
	m.Profile.Partitions = nil
	m.Profile.Reserved = []profile.Region{{Name: "keys", Offset: 0, Size: 12}}
	if err := Save(root, m); err == nil {
		if _, err := Verify(context.Background(), root); err == nil {
			t.Fatal("supported profile accepted with all partitions omitted")
		}
	}
}

func TestRawChunkVerificationGuards(t *testing.T) {
	for _, failure := range []string{"", "first hash", "second hash", "short read", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			data := []byte{1, 2, 3, 4, 5}
			hash := fmt.Sprintf("%x", sha256.Sum256(data))
			m := &Manifest{Profile: profile.Profile{Geometry: profile.Geometry{NANDSize: 4, PageSize: 4, OOBSize: 1, ChunkSize: 4}}, Chunks: []Chunk{{Size: 5, TransportVerified: true, Hashes: [2]string{hash, hash}}}}
			for _, name := range []string{"nand-full-read1.raw.bin", "nand-full-read2.raw.bin"} {
				if err := WriteFile(root, name, data); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "first hash":
				m.Chunks[0].Hashes[0] = strings.Repeat("0", 64)
			case "second hash":
				m.Chunks[0].Hashes[1] = strings.Repeat("0", 64)
			case "short read":
				if err := os.Truncate(filepath.Join(root, "nand-full-read2.raw.bin"), 4); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			err := verifyChunks(ctx, root, m)
			switch failure {
			case "":
				if err != nil {
					t.Fatal(err)
				}
			case "first hash", "second hash":
				if err == nil || !strings.Contains(err.Error(), "committed raw chunk 0 is damaged") {
					t.Fatalf("chunk hash ignored: %v", err)
				}
			case "short read":
				if !errors.Is(err, io.EOF) {
					t.Fatalf("short read accepted: %v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation ignored: %v", err)
				}
			}
		})
	}
}

func TestReadExactRejectsChangedSizeReadErrorsAndCancellation(t *testing.T) {
	if _, err := ReadExact(context.Background(), iotest.ErrReader(io.ErrClosedPipe), 4); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("read failure hidden: %v", err)
	}
	for _, text := range []string{"abc", "abcd", "abcde"} {
		data, err := ReadExact(context.Background(), strings.NewReader(text), 4)
		if text == "abcd" {
			if err != nil || string(data) != text {
				t.Fatalf("exact read: %q %v", data, err)
			}
		} else if err == nil {
			t.Fatalf("changed size accepted: %q", text)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadExact(ctx, strings.NewReader("abcd"), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := ReadExact(context.Background(), strings.NewReader(""), 0); err == nil {
		t.Fatal("empty snapshot accepted")
	}
}

func TestPreviousBackupSchemaIsRejected(t *testing.T) {
	root, m := fixture(t)
	m.Schema = 1
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("previous backup schema accepted")
	}
}
