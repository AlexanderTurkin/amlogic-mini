package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/progress"
)

type memoryDevice struct {
	p          profile.Profile
	last       []byte
	shortData  bool
	unstable   bool
	badRAM     bool
	stopAt     uint32
	stop       bool
	rawReads   int
	primeError error
}

func smallProfile() profile.Profile {
	return profile.Profile{Name: "synthetic-a113x", Geometry: profile.Geometry{NANDSize: 32, PageSize: 4, OOBSize: 1, ChunkSize: 8}, Partitions: []profile.Region{{Name: "bootloader", Offset: 0, Size: 8}, {Name: "data", Offset: 8, Size: 24}}, Reserved: []profile.Region{{Name: "keys", Offset: 0, Size: 4}}}
}
func (c *memoryDevice) Identify(context.Context) ([7]byte, error) {
	return [7]byte{0, 7, 0, 16, 0, 0, 0}, nil
}
func (c *memoryDevice) Prime(context.Context) error { return c.primeError }
func (c *memoryDevice) Environment(context.Context) ([]byte, error) {
	c.last = make([]byte, 65536)
	copy(c.last, "bootcmd=run storeboot\n")
	return bytes.Clone(c.last), nil
}
func (c *memoryDevice) ReadPartition(_ context.Context, name string, size uint32, w io.Writer) (int64, error) {
	data := bytes.Repeat([]byte(name[:1]), int(size))
	var err error
	if name == "data" && c.shortData {
		data = data[:len(data)-2]
		err = io.ErrUnexpectedEOF
	}
	n, e := w.Write(data)
	return int64(n), errors.Join(e, err)
}
func (c *memoryDevice) ReadNAND(_ context.Context, off, size uint32, raw bool) ([]byte, error) {
	if raw && c.stop && off == c.stopAt {
		return nil, context.Canceled
	}
	if raw {
		size = c.p.Geometry.RawBytes(size)
		c.rawReads++
	}
	c.last = bytes.Repeat([]byte{byte(off) + 1}, int(size))
	if raw && c.unstable && c.rawReads%2 == 0 {
		c.last[0] ^= 1
	}
	return bytes.Clone(c.last), nil
}
func (c *memoryDevice) ReadRAM(_ context.Context, _, _ uint32) ([]byte, error) {
	b := bytes.Clone(c.last)
	if c.badRAM {
		b[0] ^= 1
	}
	return b, nil
}
func opts(root string) Options {
	return Options{Output: root, Profile: smallProfile(), Version: "test"}
}

func TestBackupPersistsDifferencesAndVerifies(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dump")
	d := &memoryDevice{p: smallProfile(), unstable: true}
	m, err := Run(context.Background(), d, opts(root))
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "warnings" || len(m.Chunks) != 4 || m.Chunks[0].DifferentBytes != 1 {
		t.Fatalf("unstable NAND hidden: %+v", m)
	}
	a, _ := os.ReadFile(filepath.Join(root, "nand-full-read1.raw.bin"))
	b, _ := os.ReadFile(filepath.Join(root, "nand-full-read2.raw.bin"))
	if len(a) != 40 || len(b) != 40 || bytes.Equal(a, b) {
		t.Fatal("raw captures were altered or truncated")
	}
	if _, err := archive.Verify(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}
func TestShortPartitionRemainsPartial(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dump")
	d := &memoryDevice{p: smallProfile(), shortData: true}
	o := opts(root)
	m, err := Run(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		if f.Name == "data.bin" {
			if f.Status != "partial" || f.Size != 22 || f.Expected != 24 {
				t.Fatalf("lost partial status: %+v", f)
			}
			return
		}
	}
	t.Fatal("partial data file missing")
}
func TestRAMMismatchStopsBackup(t *testing.T) {
	d := &memoryDevice{p: smallProfile(), badRAM: true}
	root := filepath.Join(t.TempDir(), "dump")
	if _, err := Run(context.Background(), d, opts(root)); err == nil {
		t.Fatal("RAM transport corruption accepted")
	}
}
func TestInterruptedBackupCannotOverwriteExistingCapture(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dump")
	o := opts(root)
	if _, err := Run(context.Background(), &memoryDevice{p: smallProfile(), stop: true, stopAt: 8}, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), &memoryDevice{p: smallProfile()}, o); err == nil {
		t.Fatal("existing backup overwritten")
	}
	after, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed manifest")
	}
	m, err := archive.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "interrupted" || len(m.Chunks) != 1 {
		t.Fatalf("lost capture provenance: %+v", m)
	}
}

func TestEarlyFailureHasTerminalManifestStatus(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, context.Canceled} {
		root := filepath.Join(t.TempDir(), "dump")
		_, err := Run(context.Background(), &memoryDevice{p: smallProfile(), primeError: failure}, opts(root))
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		m, err := archive.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status == "running" || m.Error == "" {
			t.Fatalf("failure left nonterminal journal: %+v", m)
		}
	}
}

func TestVerifyCannotHidePhysicalDifferencesByEditingMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dump")
	o := opts(root)
	m, err := Run(context.Background(), &memoryDevice{p: smallProfile(), unstable: true}, o)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = "complete"
	m.Warnings = nil
	for i := range m.Chunks {
		m.Chunks[i].DifferentBytes = 0
		m.Chunks[i].DifferentBits = 0
		m.Chunks[i].Offsets = nil
	}
	if err := archive.Save(root, m); err != nil {
		t.Fatal(err)
	}
	if report, err := archive.Verify(context.Background(), root); err == nil && len(report.Warnings) == 0 {
		t.Fatal("unequal physical captures verified clean")
	}
}

func TestBackupReportsMeasuredTransferStages(t *testing.T) {
	o := opts(filepath.Join(t.TempDir(), "dump"))
	var events []progress.Event
	o.Progress = func(e progress.Event) { events = append(events, e) }
	if _, err := Run(context.Background(), &memoryDevice{p: smallProfile()}, o); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"read", "verify", "raw"} {
		found := false
		for _, e := range events {
			if e.Stage == stage && e.Total > 0 && e.Done == e.Total {
				found = true
			}
		}
		if !found {
			t.Fatalf("no measured %s: %+v", stage, events)
		}
	}
}
