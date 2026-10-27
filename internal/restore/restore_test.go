package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/progress"
	"amlogicmini/internal/testutil"
)

type fakeDevice struct {
	ram        []byte
	events     []string
	wrongKeys  bool
	disagree   bool
	badRead    string
	writeError string
	ramBad     bool
	mutate     func()
	cancel     context.CancelFunc
}

func (d *fakeDevice) Identify(context.Context) ([7]byte, error) { return [7]byte{0, 7, 0, 16}, nil }
func (d *fakeDevice) Prime(context.Context) error               { return nil }
func (d *fakeDevice) Environment(context.Context) ([]byte, error) {
	return []byte("bootcmd=run ka kb kc kd\n"), nil
}
func (d *fakeDevice) ReadNAND(_ context.Context, offset, size uint32, raw bool) ([]byte, error) {
	d.events = append(d.events, "keys")
	b := make([]byte, size)
	if d.wrongKeys || (d.disagree && len(d.events) > 1) {
		b[0] = 1
	}
	return b, nil
}
func (d *fakeDevice) ReadRAM(context.Context, uint32, uint32) ([]byte, error) {
	if d.ramBad {
		return []byte("bad"), nil
	}
	return bytes.Clone(d.ram), nil
}
func (d *fakeDevice) WriteRAM(_ context.Context, _ uint32, b []byte) error {
	d.ram = bytes.Clone(b)
	d.events = append(d.events, "ram")
	return nil
}
func (d *fakeDevice) ReadPartition(_ context.Context, name string, size uint32, w io.Writer) (int64, error) {
	d.events = append(d.events, "read:"+name)
	if d.badRead == name {
		return 0, io.ErrUnexpectedEOF
	}
	if name == "boot_a" || name == "boot_b" {
		return io.CopyN(w, io.MultiReader(bytes.NewReader([]byte("ANDROID!")), zeroReader{}), int64(size))
	}
	return io.CopyN(w, zeroReader{}, int64(size))
}
func (d *fakeDevice) WritePartition(_ context.Context, name string, size uint32, r io.Reader, acknowledged func(uint32)) error {
	d.events = append(d.events, "write:"+name)
	if d.writeError == name {
		return errors.New("device disconnected")
	}
	n, err := io.Copy(io.Discard, r)
	if n != int64(size) {
		return io.ErrUnexpectedEOF
	}
	if err == nil && acknowledged != nil {
		acknowledged(size)
	}
	if d.mutate != nil {
		d.mutate()
		d.mutate = nil
	}
	if d.cancel != nil {
		d.cancel()
	}
	return err
}
func (d *fakeDevice) StockBoot(context.Context) error {
	d.events = append(d.events, "stockboot")
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
func opts(s *Session, t *testing.T) Options {
	return Options{ConfirmDevice: s.Plan.Fingerprint, Report: filepath.Join(t.TempDir(), "report.json")}
}

func TestPlanListsOnlyFiveStockPartitions(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var names []string
	for _, p := range s.Plan.Partitions {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"boot_a", "boot_b", "system_a", "system_b", "data"}) {
		t.Fatal(names)
	}
	if s.Plan.Bootcmd != "run storeboot" || s.Plan.PartialData {
		t.Fatalf("plan=%+v", s.Plan)
	}
}
func TestArchiveLockExcludesAnotherRestore(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(context.Background(), dir); err == nil {
		other.Close()
		t.Fatal("concurrent archive use accepted")
	}
}
func TestApplyRefusesPartialDataBeforeDeviceAccess(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, true))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := &fakeDevice{}
	_, err = s.Apply(context.Background(), d, opts(s, t))
	if err == nil || len(d.events) != 0 {
		t.Fatalf("err=%v events=%v", err, d.events)
	}
}
func TestApplyVerifiesEveryWriteBeforeSettingStockBoot(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, true))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := &fakeDevice{}
	o := opts(s, t)
	o.AllowPartialData = true
	report, err := s.Apply(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keys", "keys", "ram", "write:boot_a", "read:boot_a", "write:boot_b", "read:boot_b", "write:system_a", "read:system_a", "write:system_b", "read:system_b", "write:data", "read:data", "stockboot"}
	if !reflect.DeepEqual(d.events, want) {
		t.Fatalf("sequence=%v", d.events)
	}
	if report.Status != "verified-with-partial-data" || len(report.Partitions) != 5 || !report.StockBootVerified {
		t.Fatalf("report=%+v", report)
	}
	if _, err := os.Stat(o.Report); err != nil {
		t.Fatal(err)
	}
	for _, p := range report.Partitions {
		if p.ReadbackSHA256 != p.SHA256 || p.Status != "verified" {
			t.Fatal(p)
		}
	}
}
func TestFailuresStopBeforeStockBoot(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	for _, kind := range []string{"wrong-keys", "disagreeing-keys", "RAM", "write", "readback", "confirmation", "report", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			d := &fakeDevice{}
			o := opts(s, t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "wrong-keys":
				d.wrongKeys = true
			case "disagreeing-keys":
				d.disagree = true
			case "RAM":
				d.ramBad = true
			case "write":
				d.writeError = "boot_a"
			case "readback":
				d.badRead = "boot_a"
			case "confirmation":
				o.ConfirmDevice = "wrong"
			case "report":
				o.Report = filepath.Join(t.TempDir(), "missing", "report.json")
			case "cancel":
				cancel()
			}
			_, err = s.Apply(ctx, d, o)
			if err == nil {
				t.Fatal("failure accepted")
			}
			for _, event := range d.events {
				if event == "stockboot" || event == "write:boot_b" {
					t.Fatalf("continued: %v", d.events)
				}
			}
		})
	}
}
func TestSourceMutationIsCaughtBeforeWritingChangedPartition(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := &fakeDevice{mutate: func() {
		f, err := os.OpenFile(filepath.Join(dir, "boot_b.bin"), os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte{1}, 0)
		f.Close()
	}}
	_, err = s.Apply(context.Background(), d, opts(s, t))
	if err == nil {
		t.Fatal("mutation accepted")
	}
	for _, event := range d.events {
		if event == "write:boot_b" || event == "stockboot" {
			t.Fatalf("continued: %v", d.events)
		}
	}
}
func TestRejectsResumedOrNonDoubleReadArchive(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	m, err := archive.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.ResumeCount = 1
	archive.Save(dir, m)
	if s, err := Open(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("resumed archive accepted")
	}
	m.ResumeCount = 0
	for i := range m.Files {
		if m.Files[i].Name == "boot_a.bin" {
			m.Files[i].DoubleRead = false
		}
	}
	archive.Save(dir, m)
	if s, err := Open(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("unverified partition accepted")
	}
}

func TestReportFailureStopsBeforeNextOperation(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	o := opts(s, t)
	dir := filepath.Dir(o.Report)
	d := &fakeDevice{mutate: func() {
		if err := os.Rename(dir, dir+"-offline"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("unavailable"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	report, err := s.Apply(context.Background(), d, o)
	if err == nil || report == nil || report.Status != "failed" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	for _, event := range d.events {
		if event == "read:boot_a" || event == "stockboot" {
			t.Fatal(d.events)
		}
	}
}
func TestPublicPlanCannotSelectProtectedRegions(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Plan.Partitions[0].Name = "bootloader"
	s.Plan.Partitions[0].Offset = 0
	d := &fakeDevice{writeError: "boot_a"}
	_, err = s.Apply(context.Background(), d, opts(s, t))
	if err == nil {
		t.Fatal("expected interrupted write")
	}
	for _, event := range d.events {
		if event == "write:bootloader" {
			t.Fatal("public plan changed restore targets")
		}
	}
}
func TestInvalidArchiveFailsBeforePlanning(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	if err := os.WriteFile(filepath.Join(dir, "system_a.bin"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("damaged source accepted")
	}
}

func TestDisappearingOrReplacedReportStorageStopsRestore(t *testing.T) {
	dir := testutil.StockBackup(t, false)
	for _, kind := range []string{"directory-replaced", "report-deleted", "report-replaced"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			o := opts(s, t)
			d := &fakeDevice{mutate: func() {
				switch kind {
				case "directory-replaced":
					parent := filepath.Dir(o.Report)
					if err := os.Rename(parent, parent+"-unmounted"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(parent, 0700); err != nil {
						t.Fatal(err)
					}
				case "report-deleted":
					if err := os.Remove(o.Report); err != nil {
						t.Fatal(err)
					}
				case "report-replaced":
					if err := os.Rename(o.Report, o.Report+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(o.Report, []byte("replacement"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}}
			report, err := s.Apply(context.Background(), d, o)
			if err == nil || report == nil || report.Status != "failed" {
				t.Fatalf("storage change accepted: status=%s err=%v", report.Status, err)
			}
			for _, event := range d.events {
				if event == "read:boot_a" || event == "write:boot_b" || event == "stockboot" {
					t.Fatalf("continued after report loss: %v", d.events)
				}
			}
		})
	}
}

func rewriteArchiveFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if name == "env.txt" {
		if err := f.Truncate(int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := archive.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash, size, err := archive.HashFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Files {
		if m.Files[i].Name == name {
			m.Files[i].SHA256 = hash
			m.Files[i].Size = size
			if name == "env.txt" {
				m.Files[i].Expected = size
			}
		}
	}
	if err := archive.Save(dir, m); err != nil {
		t.Fatal(err)
	}
}
func TestRestoreRejectsNonStockSnapshotWithValidHashes(t *testing.T) {
	for _, kind := range []string{"alpine-env", "kernel-a", "kernel-b", "duplicate-command"} {
		t.Run(kind, func(t *testing.T) {
			dir := testutil.StockBackup(t, false)
			switch kind {
			case "alpine-env":
				rewriteArchiveFile(t, dir, "env.txt", []byte("bootcmd=run ka kb kc kd\n"))
			case "kernel-a":
				rewriteArchiveFile(t, dir, "boot_a.bin", []byte("ARMd...."))
			case "kernel-b":
				rewriteArchiveFile(t, dir, "boot_b.bin", []byte("ARMd...."))
			case "duplicate-command":
				rewriteArchiveFile(t, dir, "env.txt", []byte("bootcmd=run storeboot\nbootcmd=run ka kb kc kd\n"))
			}
			if s, err := Open(context.Background(), dir); err == nil {
				s.Close()
				t.Fatal("non-stock snapshot accepted for stock restore")
			}
		})
	}
}

type unacknowledgedDevice struct{ *fakeDevice }

func (d unacknowledgedDevice) WritePartition(_ context.Context, _ string, _ uint32, source io.Reader, _ func(uint32)) error {
	if _, err := io.Copy(io.Discard, source); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}
func TestRestoreProgressCannotFinishOnUnacknowledgedTransfer(t *testing.T) {
	s, err := Open(context.Background(), testutil.StockBackup(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	o := opts(s, t)
	var finished bool
	o.Progress = func(e progress.Event) {
		if e.Stage == "write" && e.Total > 0 && e.Done == e.Total {
			finished = true
		}
	}
	_, err = s.Apply(context.Background(), unacknowledgedDevice{&fakeDevice{}}, o)
	if err == nil || finished {
		t.Fatalf("source consumption claimed completed USB transfer: %v, %v", finished, err)
	}
}
