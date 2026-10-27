package flash

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amlogicmini/internal/testutil"
)

type fakeDevice struct {
	badBootRead         bool
	badLoader           bool
	events              []string
	ram                 []byte
	written             map[string][]byte
	wrongKeys           bool
	failWrite, failRead string
	afterWrite          func()
	onWrite             func(string)
}

func (d *fakeDevice) Identify(context.Context) ([7]byte, error) {
	if d.badLoader {
		return [7]byte{}, errors.New("unsupported loader")
	}
	return [7]byte{0, 7, 0, 16}, nil
}
func (d *fakeDevice) Prime(context.Context) error { return nil }
func (d *fakeDevice) Environment(context.Context) ([]byte, error) {
	return []byte("bootcmd=run storeboot\n"), nil
}
func (d *fakeDevice) ReadNAND(_ context.Context, offset, size uint32, raw bool) ([]byte, error) {
	if offset == 0x400000 {
		b := make([]byte, size)
		if d.wrongKeys {
			b[0] = 1
		}
		return b, nil
	}
	name := ""
	base := uint32(0)
	switch {
	case offset >= 0x2c00000:
		name = "ubi"
		base = 0x2c00000
	case offset >= 0x2b00000:
		name = "dtb"
		base = 0x2b00000
	case offset >= 0x1200000:
		name = "kernel"
		base = 0x1200000
	default:
		return nil, errors.New("protected NAND")
	}
	d.events = append(d.events, "read:"+name)
	if d.failRead == name {
		return nil, io.ErrUnexpectedEOF
	}
	out := make([]byte, size)
	copy(out, d.written[name][offset-base:])
	return out, nil
}
func (d *fakeDevice) ReadRAM(context.Context, uint32, uint32) ([]byte, error) {
	return bytes.Clone(d.ram), nil
}
func (d *fakeDevice) WriteRAM(_ context.Context, _ uint32, b []byte) error {
	d.ram = bytes.Clone(b)
	return nil
}
func (d *fakeDevice) EraseAlpine(context.Context) error {
	d.events = append(d.events, "erase")
	d.written = map[string][]byte{}
	return nil
}
func (d *fakeDevice) WriteAlpineChunk(_ context.Context, name string, offset uint32, b []byte) error {
	d.events = append(d.events, "write:"+name)
	if d.failWrite == name {
		return errors.New("disconnected")
	}
	if uint32(len(d.written[name])) != offset {
		return errors.New("wrong image offset")
	}
	d.written[name] = append(d.written[name], b...)
	if d.onWrite != nil {
		d.onWrite(name)
	}
	if d.afterWrite != nil {
		f := d.afterWrite
		d.afterWrite = nil
		f()
	}
	return nil
}
func (d *fakeDevice) AlpineBoot(context.Context, uint32, uint32) error {
	d.events = append(d.events, "alpineboot")
	return nil
}

func TestFlashWritesAllImagesThenVerifiesAllBeforeBoot(t *testing.T) {
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{}
	o := Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "flash.json")}
	report, err := s.Apply(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"erase", "write:kernel", "write:dtb", "write:ubi", "bootread:kernel", "bootread:dtb", "read:ubi", "alpineboot"}
	if !reflect.DeepEqual(d.events, want) {
		t.Fatal(d.events)
	}
	if report.Status != "verified" || !report.BootEnvironmentVerified {
		t.Fatalf("status=%s", report.Status)
	}
	for _, image := range report.Images {
		if image.Status != "verified" || image.SHA256 != image.ReadbackSHA256 {
			t.Fatal(image)
		}
	}
	if len(d.written["kernel"]) != 2048 || bytes.Count(d.written["kernel"][64:], []byte{255}) != 1984 {
		t.Fatal("kernel padding is not ff")
	}
}
func TestApplyFailuresNeverSwitchBootEnvironment(t *testing.T) {
	images, backup := testutil.Firmware(t), t.TempDir()
	for _, kind := range []string{"loader", "write", "readback", "report", "confirmation", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, err := OpenWithBackupPresence(context.Background(), images, backup)
			if err != nil {
				t.Fatal(err)
			}
			d := &fakeDevice{}
			o := Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "flash.json")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "loader":
				d.badLoader = true
			case "write":
				d.failWrite = "kernel"
			case "readback":
				d.failRead = "kernel"
			case "report":
				o.Report = filepath.Join(images, "report.json")
			case "confirmation":
				o.BackupPresenceConfirmed = false
			case "cancel":
				cancel()
			}
			_, err = s.Apply(ctx, d, o)
			if err == nil {
				t.Fatal("failure accepted")
			}
			for _, event := range d.events {
				if event == "alpineboot" {
					t.Fatal(d.events)
				}
			}
			if kind == "loader" || kind == "report" || kind == "confirmation" || kind == "cancel" {
				if len(d.events) != 0 {
					t.Fatal("wrote before preflight", d.events)
				}
			}
		})
	}
}
func TestImageMutationBeforeApplyPreventsErase(t *testing.T) {
	images := testutil.Firmware(t)
	s, err := OpenWithBackupPresence(context.Background(), images, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(images, "Image"), []byte("changed"), 0600)
	d := &fakeDevice{}
	_, err = s.Apply(context.Background(), d, Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "flash.json")})
	if err == nil || len(d.events) != 0 {
		t.Fatalf("mutation accepted: %v %v", err, d.events)
	}
}
func TestReportLossDuringFlashStopsSubsequentWrites(t *testing.T) {
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	o := Options{BackupPresenceConfirmed: true, Report: filepath.Join(parent, "flash.json")}
	d := &fakeDevice{afterWrite: func() { os.Rename(parent, parent+"-offline"); os.Mkdir(parent, 0700) }}
	_, err = s.Apply(context.Background(), d, o)
	if err == nil {
		t.Fatal("report loss ignored")
	}
	if !reflect.DeepEqual(d.events, []string{"erase", "write:kernel"}) {
		t.Fatal(d.events)
	}
}

func TestLaterWriteCannotHideEarlierImageCorruption(t *testing.T) {
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{}
	d.onWrite = func(name string) {
		if name == "dtb" {
			d.written["kernel"][0] ^= 1
		}
	}
	_, err = s.Apply(context.Background(), d, Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "flash.json")})
	if err == nil {
		t.Fatal("cross-image corruption accepted")
	}
	for _, event := range d.events {
		if event == "alpineboot" {
			t.Fatal("boot configured before global verification")
		}
	}
}

func (d *fakeDevice) ReadAlpineBootImage(_ context.Context, name string, size uint32, out io.Writer) (int64, error) {
	d.events = append(d.events, "bootread:"+name)
	if d.failRead == name {
		return 0, io.ErrUnexpectedEOF
	}
	if d.badBootRead {
		return 0, errors.New("continuous boot read differs")
	}
	n, err := out.Write(d.written[name][:size])
	return int64(n), err
}
func TestChunkAliasCannotPassBootVerification(t *testing.T) {
	// NAND block10 is bad. Old fixed-offset chunks both match when logical
	// blocks63/64 are equal, but a boot-time continuous read loses block64.
	source := make([]int, 128)
	for i := range source {
		source[i] = i
	}
	source[64] = source[63]
	physical := map[int]int{}
	good := func(block int) int {
		if block == 10 {
			return 11
		}
		return block
	}
	for start := 0; start < 128; start += 64 {
		p := start
		for _, value := range source[start : start+64] {
			p = good(p)
			physical[p] = value
			p++
		}
	}
	read := func(start, count int) []int {
		result := make([]int, 0, count)
		for len(result) < count {
			start = good(start)
			result = append(result, physical[start])
			start++
		}
		return result
	}
	if !reflect.DeepEqual(read(0, 64), source[:64]) || !reflect.DeepEqual(read(64, 64), source[64:]) {
		t.Fatal("fixture does not reproduce old false success")
	}
	if reflect.DeepEqual(read(0, 128), source) {
		t.Fatal("fixture continuous read must differ")
	}
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{badBootRead: true}
	_, err = s.Apply(context.Background(), d, Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "flash.json")})
	if err == nil {
		t.Fatal("flash accepted a failed continuous boot read")
	}
	for _, event := range d.events {
		if event == "alpineboot" {
			t.Fatal("boot switched despite continuous read failure")
		}
	}
}

func TestPresenceOnlyFlashDoesNotReadBackupContents(t *testing.T) {
	saved := t.TempDir()
	if err := os.WriteFile(filepath.Join(saved, "manifest.json"), []byte("not a manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), saved)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{wrongKeys: true}
	report, err := s.Apply(context.Background(), d, Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "operation.json")})
	if err != nil || report.Status != "verified" || report.Plan.Fingerprint == "" {
		t.Fatalf("presence-only flash: %v %+v", err, report)
	}
}
func TestPresenceOnlyFlashRequiresConfirmationAndExistingDirectory(t *testing.T) {
	for _, mode := range []string{"not confirmed", "backup removed"} {
		t.Run(mode, func(t *testing.T) {
			saved := t.TempDir()
			s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), saved)
			if err != nil {
				t.Fatal(err)
			}
			d := &fakeDevice{}
			o := Options{BackupPresenceConfirmed: mode != "not confirmed", Report: filepath.Join(t.TempDir(), "operation.json")}
			if mode == "backup removed" {
				if err := os.Remove(saved); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Apply(context.Background(), d, o); err == nil {
				t.Fatal("applied without backup presence or confirmation")
			}
			if len(d.events) != 0 {
				t.Fatal("NAND touched before prerequisites")
			}
		})
	}
}

type presenceBadRAM struct{ *fakeDevice }

func (d presenceBadRAM) ReadRAM(context.Context, uint32, uint32) ([]byte, error) {
	return []byte{1}, nil
}
func TestPresenceOnlyFlashStillTestsUSBAndRAM(t *testing.T) {
	s, err := OpenWithBackupPresence(context.Background(), testutil.Firmware(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := presenceBadRAM{&fakeDevice{}}
	if _, err := s.Apply(context.Background(), d, Options{BackupPresenceConfirmed: true, Report: filepath.Join(t.TempDir(), "operation.json")}); err == nil {
		t.Fatal("broken RAM accepted")
	}
	if len(d.events) != 0 {
		t.Fatal("erased before RAM check")
	}
}
