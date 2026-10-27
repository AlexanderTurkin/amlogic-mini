package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/progress"
)

type Device interface {
	Identify(context.Context) ([7]byte, error)
	Prime(context.Context) error
	Environment(context.Context) ([]byte, error)
	ReadPartition(context.Context, string, uint32, io.Writer) (int64, error)
	ReadNAND(context.Context, uint32, uint32, bool) ([]byte, error)
	ReadRAM(context.Context, uint32, uint32) ([]byte, error)
}

type Options struct {
	Output   string
	Profile  profile.Profile
	Firmware [7]byte
	Version  string
	Progress progress.Notify
}

type session struct {
	device   Device
	options  Options
	manifest *archive.Manifest
}

func Run(ctx context.Context, d Device, o Options) (result *archive.Manifest, err error) {
	if o.Output == "" {
		return nil, fmt.Errorf("backup output is required")
	}
	if err := o.Profile.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Output, 0700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(o.Output, archive.LockFileName)
	if _, e := os.Lstat(lockPath); e == nil {
		if e = archive.EnsureRegular(lockPath); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	lock := flock.New(lockPath)
	locked, e := lock.TryLock()
	if e != nil {
		return nil, e
	}
	if !locked {
		lock.Close()
		return nil, fmt.Errorf("another process is using this backup")
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	entries, e := os.ReadDir(o.Output)
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		if entry.Name() != archive.LockFileName {
			return nil, fmt.Errorf("output directory is not empty; use a new directory")
		}
	}
	m := &archive.Manifest{Schema: archive.SchemaVersion, Version: o.Version, Profile: o.Profile, Mode: "all", Status: "running", Started: time.Now().UTC().Format(time.RFC3339), Firmware: o.Firmware}
	if err := archive.Save(o.Output, m); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			m.Status = "failed"
			if errors.Is(err, context.Canceled) {
				m.Status = "interrupted"
			}
			m.Error = err.Error()
			err = errors.Join(err, archive.Save(o.Output, m))
		}
	}()
	s := &session{device: d, options: o, manifest: m}
	s.progress(progress.Event{Stage: "check"})
	version, e := d.Identify(ctx)
	if e != nil {
		return nil, e
	}
	if o.Firmware != [7]byte{} && version != o.Firmware {
		return nil, fmt.Errorf("bootloader changed after USB capture")
	}
	if err := d.Prime(ctx); err != nil {
		return nil, err
	}
	var keys profile.Region
	for _, r := range o.Profile.Reserved {
		if r.Name == "keys" {
			keys = r
			break
		}
	}
	if keys.Size == 0 {
		return nil, fmt.Errorf("profile has no individual key region")
	}
	keyData, e := s.readRegion(ctx, keys)
	if e != nil {
		return nil, fmt.Errorf("device fingerprint: %w", e)
	}
	fingerprint := sum(keyData)
	m.Identity = fingerprint
	m.Firmware = version
	m.Status = "running"
	m.Error = ""
	m.Finished = ""
	if err = s.recordBytes(ctx, "rsv-keys.bin", keyData, true); err != nil {
		return m, err
	}
	if err = s.partitions(ctx); err != nil {
		return m, err
	}
	if err = s.raw(ctx); err != nil {
		return m, err
	}
	m.Warnings = nil
	for _, file := range m.Files {
		if file.Status == "partial" {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s is partial (%d/%d bytes)", file.Name, file.Size, file.Expected))
		}
	}
	var changed int
	for _, chunk := range m.Chunks {
		changed += chunk.DifferentBytes
	}
	if changed > 0 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("physical NAND captures differ in %d bytes; both unmodified captures retained", changed))
	}
	m.Status = "complete"
	if len(m.Warnings) > 0 {
		m.Status = "warnings"
	}
	m.Finished = time.Now().UTC().Format(time.RFC3339)
	if err = archive.Save(o.Output, m); err != nil {
		return m, err
	}
	var sums strings.Builder
	files := append([]archive.File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, file := range files {
		fmt.Fprintf(&sums, "%s  %s\n", file.SHA256, file.Name)
	}
	if err = archive.ReplaceFile(o.Output, "SHA256SUMS", []byte(sums.String())); err != nil {
		return m, err
	}
	s.progress(progress.Event{Stage: "files"})
	_, err = archive.Verify(ctx, o.Output)
	return m, err
}

func sum(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func (s *session) progress(message progress.Event) {
	if s.options.Progress != nil {
		s.options.Progress(message)
	}
}
func (s *session) save() error { return archive.Save(s.options.Output, s.manifest) }

func (s *session) record(file archive.File) error {
	for i, old := range s.manifest.Files {
		if old.Name == file.Name {
			s.manifest.Files[i] = file
			return s.save()
		}
	}
	s.manifest.Files = append(s.manifest.Files, file)
	return s.save()
}
func (s *session) recordBytes(ctx context.Context, name string, data []byte, double bool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty snapshot: %s", name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archive.ReplaceFile(s.options.Output, name, data); err != nil {
		return err
	}
	return s.record(archive.File{Name: name, Size: int64(len(data)), Expected: int64(len(data)), SHA256: sum(data), Status: "verified", DoubleRead: double})
}

func (s *session) readRegion(ctx context.Context, r profile.Region) ([]byte, error) {
	var first []byte
	for pass := 0; pass < 2; pass++ {
		data, err := s.device.ReadNAND(ctx, r.Offset, r.Size, false)
		if err != nil {
			return nil, err
		}
		if len(data) != int(r.Size) {
			return nil, fmt.Errorf("short reserved region %s", r.Name)
		}
		again, err := s.device.ReadRAM(ctx, 0x01080000, r.Size)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(data, again) {
			return nil, fmt.Errorf("RAM/USB transfer mismatch in %s", r.Name)
		}
		if pass == 0 {
			first = data
		} else if !bytes.Equal(first, data) {
			return nil, fmt.Errorf("two NAND reads differ in reserved region %s", r.Name)
		}
	}
	return first, nil
}
