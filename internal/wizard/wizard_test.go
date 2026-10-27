package wizard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amlogicmini/internal/backup"
	"amlogicmini/internal/onboarding"
	"amlogicmini/internal/testutil"
)

type script struct {
	values  []string
	secrets int
}

func (s *script) Ask(ctx context.Context, p Prompt) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(s.values) == 0 {
		return "", ErrInputClosed
	}
	v := s.values[0]
	s.values = s.values[1:]
	if p.Secret {
		s.secrets++
	}
	if p.Validate != nil {
		if err := p.Validate(v); err != nil {
			return "", err
		}
	}
	return v, nil
}
func options(t *testing.T, values ...string) (Options, *bytes.Buffer, *int) {
	t.Helper()
	out := new(bytes.Buffer)
	opens := new(int)
	return Options{Home: t.TempDir(), Out: out, Prompts: &script{values: values}, Open: func(context.Context, time.Duration) (backup.Device, [7]byte, func() error, error) {
		*opens++
		return nil, [7]byte{}, nil, errors.New("TEST: USB must not be opened")
	}}, out, opens
}
func TestMenuExitAndEOFNeverOpenUSB(t *testing.T) {
	for _, values := range [][]string{{"", "0"}, nil, {""}} {
		o, out, n := options(t, values...)
		if err := Run(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if *n != 0 || !strings.Contains(out.String(), "CLI-инструмент для прошивки Alpine Linux.") {
			t.Fatal(out.String())
		}
	}
}
func TestMissingBackupBlocksFlashBeforeUSB(t *testing.T) {
	o, out, n := options(t, "", "3", "0")
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "нет резервных копий") {
		t.Fatalf("opened %d: %s", *n, out)
	}
}
func TestInvalidAdjacentBackupDoesNotOpenUSB(t *testing.T) {
	o, out, n := options(t, "", "2", "1", "0")
	dir := filepath.Join(o.Home, "backups", "damaged")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "Ошибка") {
		t.Fatalf("opened %d: %s", *n, out)
	}
}
func TestRestoreDefaultCancellationDoesNotOpenUSB(t *testing.T) {
	saved := testutil.StockBackup(t, true)
	o, _, n := options(t, "", "2", "1", "", "0")
	installBackup(t, o.Home, saved)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 {
		t.Fatal("default confirmation opened USB")
	}
}
func TestFlashCancellationLeavesBackupAndFirmwareUnchanged(t *testing.T) {
	saved, images := testutil.StockBackup(t, true), testutil.Firmware(t)
	before, err := os.ReadFile(filepath.Join(saved, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	o, _, n := options(t, "", "3", "", "0")
	saved = installBackup(t, o.Home, saved)
	if err := os.Rename(images, filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(saved, "manifest.json"))
	if *n != 0 || !bytes.Equal(before, after) {
		t.Fatal("cancelled flash touched source or USB")
	}
}
func TestPortableStoragePreservesSpacesAndUnicode(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	o.Home = filepath.Join(o.Home, "Моё устройство с пробелами")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "[OK] Образы Alpine проверены") {
		t.Fatal(out.String())
	}
}

type setupSSH struct {
	waits    int
	changes  int
	fail     bool
	password string
	key      string
}

func (s *setupSSH) Wait(ctx context.Context, p string) (onboarding.Info, error) {
	s.waits++
	s.password = p
	if s.fail {
		return onboarding.Info{}, onboarding.ErrAuthentication
	}
	key := s.key
	if key == "" {
		key = "SHA256:test"
	}
	return onboarding.Info{Version: "3.24.1", HostKey: key}, nil
}
func (s *setupSSH) ChangePassword(context.Context, string, string) error {
	s.changes++
	return &onboarding.PasswordError{Cause: errors.New("connection lost")}
}
func TestPostFlashSkipDoesNotClaimSSHSuccess(t *testing.T) {
	o, out, _ := options(t, "0")
	s := &setupSSH{}
	o.NewSSH = func() SSH { return s }
	w := newWizard(o)
	if err := w.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.waits != 0 || strings.Contains(out.String(), "SSH проверен") {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "ssh root@192.168.14.14") {
		t.Fatal("missing connection command")
	}
}
func TestPostFlashChecksLoginBeforeOfferingHiddenPasswordChange(t *testing.T) {
	o, out, _ := options(t, "1", "1", "long-new-secret", "long-new-secret", "0")
	s := &setupSSH{}
	o.NewSSH = func() SSH { return s }
	w := newWizard(o)
	if err := w.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.waits != 1 || s.changes != 1 || o.Prompts.(*script).secrets != 2 {
		t.Fatalf("%+v %s", s, out)
	}
	if !strings.Contains(out.String(), "мог уже измениться") || strings.Contains(out.String(), "long-new-secret") || strings.Contains(out.String(), "Пароль изменён.") {
		t.Fatal(out.String())
	}
}
func TestSSHFailureAllowsSkipWithoutReflash(t *testing.T) {
	o, out, n := options(t, "1", "0")
	s := &setupSSH{fail: true}
	o.NewSSH = func() SSH { return s }
	if err := newWizard(o).setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.waits != 1 || s.changes != 0 || *n != 0 || strings.Contains(out.String(), "SSH проверен") {
		t.Fatal(out.String())
	}
}

// A memory-only device lets the real flash engine enforce identity, reports and
// readback while the wizard test exercises its orchestration.
type memoryDevice struct {
	ram                  []byte
	images               map[string][]byte
	wrongKeys, failWrite bool
	erased, closed       bool
}

func (d *memoryDevice) Identify(context.Context) ([7]byte, error) { return [7]byte{0, 7, 0, 16}, nil }
func (d *memoryDevice) Prime(context.Context) error               { return nil }
func (d *memoryDevice) Environment(context.Context) ([]byte, error) {
	return []byte("bootcmd=run storeboot\n"), nil
}
func (d *memoryDevice) ReadPartition(context.Context, string, uint32, io.Writer) (int64, error) {
	return 0, errors.New("unexpected stock read")
}
func (d *memoryDevice) ReadRAM(context.Context, uint32, uint32) ([]byte, error) {
	return bytes.Clone(d.ram), nil
}
func (d *memoryDevice) WriteRAM(_ context.Context, _ uint32, b []byte) error {
	d.ram = bytes.Clone(b)
	return nil
}
func (d *memoryDevice) WritePartition(context.Context, string, uint32, io.Reader, func(uint32)) error {
	return errors.New("unexpected stock write")
}
func (d *memoryDevice) StockBoot(context.Context) error { return errors.New("unexpected stock boot") }
func (d *memoryDevice) ReadNAND(_ context.Context, offset, size uint32, _ bool) ([]byte, error) {
	if offset == 0x400000 {
		b := make([]byte, size)
		if d.wrongKeys {
			b[0] = 1
		}
		return b, nil
	}
	if offset == 0x2c00000 {
		return bytes.Clone(d.images["ubi"][:size]), nil
	}
	return nil, errors.New("unexpected NAND read")
}
func (d *memoryDevice) EraseAlpine(context.Context) error {
	d.erased = true
	d.images = map[string][]byte{}
	return nil
}
func (d *memoryDevice) WriteAlpineChunk(_ context.Context, name string, _ uint32, b []byte) error {
	if d.failWrite {
		return errors.New("USB lost")
	}
	d.images[name] = append(d.images[name], b...)
	return nil
}
func (d *memoryDevice) ReadAlpineBootImage(_ context.Context, name string, size uint32, out io.Writer) (int64, error) {
	n, err := out.Write(d.images[name][:size])
	return int64(n), err
}
func (d *memoryDevice) AlpineBoot(context.Context, uint32, uint32) error { return nil }
func TestFlashChecksSameDeviceAndClosesUSBBeforeSetup(t *testing.T) {
	for _, mode := range []string{"success", "foreign", "failed write"} {
		t.Run(mode, func(t *testing.T) {
			o, out, _ := options(t, "", "3", "1", "0", "0")
			installBackup(t, o.Home, testutil.StockBackup(t, true))
			if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
				t.Fatal(err)
			}
			d := &memoryDevice{wrongKeys: mode == "foreign", failWrite: mode == "failed write"}
			o.Open = func(context.Context, time.Duration) (backup.Device, [7]byte, func() error, error) {
				return d, [7]byte{0, 7, 0, 16}, func() error { d.closed = true; return nil }, nil
			}
			if err := Run(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if !d.closed {
				t.Fatal("USB leak")
			}
			if mode == "foreign" && !d.erased {
				t.Fatal("menu unexpectedly read backup identity under existence-only policy")
			}
			setup := strings.Contains(out.String(), "ПРОВЕРКА ЗАПУСКА ALPINE")
			if setup != (mode != "failed write") {
				t.Fatalf("setup=%v %s", setup, out)
			}
			if _, err := os.Stat(filepath.Join(o.Home, "reports")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("menu left a reports folder beside the binary")
			}
			if strings.Contains(out.String(), "Отчёт:") {
				t.Fatal("menu displayed internal operation report")
			}

		})
	}
}
func TestPartialRestoreUsesOnlyNormalConfirmation(t *testing.T) {
	saved := testutil.StockBackup(t, true)
	o, out, n := options(t, "", "2", "1", "1", "0")
	installBackup(t, o.Home, saved)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 1 || !strings.Contains(out.String(), "ВОССТАНОВИТЬ УСТРОЙСТВО?") || strings.Contains(out.String(), "НЕПОЛНОГО DATA") {
		t.Fatal("restore asked for an extra partial-data confirmation")
	}
}
func TestCancellationOfBackupCreatesNoFiles(t *testing.T) {
	o, _, n := options(t, "", "1", "", "0")
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 {
		t.Fatal("USB opened before confirmation")
	}
	entries, err := os.ReadDir(o.Home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%v %v", entries, err)
	}
}

func installBackup(t *testing.T, home, saved string) string {
	t.Helper()
	root := filepath.Join(home, "backups")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, filepath.Base(saved))
	if err := os.Rename(saved, target); err != nil {
		t.Fatal(err)
	}
	return target
}
func TestBackupAutomaticallyReservesNewDirectoryBesideProgram(t *testing.T) {
	o, out, _ := options(t, "", "1", "1", "0")
	var destination string
	o.Open = func(context.Context, time.Duration) (backup.Device, [7]byte, func() error, error) {
		dirs, err := os.ReadDir(filepath.Join(o.Home, "backups"))
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != 1 {
			t.Fatalf("backup directories: %d", len(dirs))
		}
		destination = filepath.Join(o.Home, "backups", dirs[0].Name())
		return nil, [7]byte{}, nil, errors.New("test stopped before capture")
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if destination == "" || !strings.Contains(out.String(), destination) {
		t.Fatalf("automatic destination missing: %s", out)
	}
}
func TestFlashAutomaticallyValidatesAdjacentAlpineWithoutPathInput(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "[OK] Образы Alpine проверены") {
		t.Fatalf("adjacent images not used: %s", out)
	}
}
func TestMissingAdjacentFirmwareStopsBeforeUSB(t *testing.T) {
	o, out, n := options(t, "", "3", "0")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "alpine") {
		t.Fatalf("missing firmware: %s", out)
	}
}
func TestBackupPickerOffersEverySavedCopyWithoutPaths(t *testing.T) {
	o, _, _ := options(t, "9", "1")
	for i := 1; i <= 9; i++ {
		dir := filepath.Join(o.Home, "backups", fmt.Sprintf("2026-10-%02d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := newWizard(o).selectBackup(context.Background())
	if err != nil || filepath.Base(got) != "2026-10-01" {
		t.Fatalf("oldest copy inaccessible: %q %v", got, err)
	}
}

func TestEmptyManifestReportsErrorAndReturnsToMenu(t *testing.T) {
	o, out, _ := options(t, "", "2", "1", "0")
	dir := filepath.Join(o.Home, "backups", "empty")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Ошибка:") || strings.Count(out.String(), "ГЛАВНОЕ МЕНЮ") != 2 {
		t.Fatal("source EOF was silently treated as cancellation")
	}
}
func TestPartialBackupUsesOnlyNormalFlashConfirmation(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || strings.Contains(out.String(), "неполный") || strings.Contains(out.String(), "НЕПОЛНОГО DATA") || !strings.Contains(out.String(), "УСТАНОВИТЬ ALPINE LINUX?") {
		t.Fatal("partial backup cluttered normal confirmation")
	}
}
func TestEachSetupUsesFreshSSHSession(t *testing.T) {
	o, out, _ := options(t, "1", "0", "1", "0")
	instances := 0
	o.NewSSH = func() SSH { instances++; return &setupSSH{key: fmt.Sprintf("SHA256:setup%d", instances)} }
	w := newWizard(o)
	for i := 0; i < 2; i++ {
		if err := w.setup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "SHA256:setup1") || !strings.Contains(out.String(), "SHA256:setup2") {
		t.Fatal("separate installs reused one pinned SSH session")
	}
}

func TestWelcomeShowsOnlyProjectNameAndAffiliation(t *testing.T) {
	o, out, _ := options(t)
	newWizard(o).welcome()
	if !strings.Contains(out.String(), "Amlogic A113X Mini") || !strings.Contains(out.String(), "CLI-инструмент для прошивки Alpine Linux.") {
		t.Fatal("missing project identity")
	}
	for _, removed := range []string{"Сделано с благодарностью", "Независимый проект.", "страх и риск", "К А П С У Л А"} {
		if strings.Contains(out.String(), removed) {
			t.Fatalf("old welcome copy remains: %s", removed)
		}
	}
}

func TestFlashDoesNotAskToChooseBackup(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || strings.Contains(out.String(), "ВЫБОР РЕЗЕРВНОЙ КОПИИ") || !strings.Contains(out.String(), "УСТАНОВИТЬ ALPINE LINUX?") {
		t.Fatal("flash still asks for a backup selection")
	}
}
func TestFlashDoesNotReadCandidateManifest(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	installBackup(t, o.Home, testutil.StockBackup(t, true))
	damaged := filepath.Join(o.Home, "backups", "zz-newer-damaged")
	if err := os.MkdirAll(damaged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(damaged, "manifest.json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "[OK] Резервная копия найдена") {
		t.Fatal("valid backup was not found automatically")
	}
}
func TestFlashOnlyChecksBackupDirectoryExists(t *testing.T) {
	o, out, n := options(t, "", "3", "", "0")
	dir := filepath.Join(o.Home, "backups", "existing-copy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *n != 0 || !strings.Contains(out.String(), "УСТАНОВИТЬ ALPINE LINUX?") || strings.Contains(out.String(), "ВЫБОР РЕЗЕРВНОЙ КОПИИ") {
		t.Fatal("backup contents were checked instead of presence")
	}
}

func TestRunActionUsesTheSameThreeOperations(t *testing.T) {
	for _, action := range []string{"backup", "restore", "flash"} {
		t.Run(action, func(t *testing.T) {
			o, out, n := options(t, "")
			if action == "restore" {
				installBackup(t, o.Home, testutil.StockBackup(t, true))
			}
			if action == "flash" {
				dir := filepath.Join(o.Home, "backups", "present")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(testutil.Firmware(t), filepath.Join(o.Home, "alpine")); err != nil {
					t.Fatal(err)
				}
			}
			if err := RunAction(context.Background(), action, o); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"backup": "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?", "restore": "ВЫБОР РЕЗЕРВНОЙ КОПИИ", "flash": "УСТАНОВИТЬ ALPINE LINUX?"}[action]
			if *n != 0 || !strings.Contains(out.String(), want) || strings.Contains(out.String(), "ГЛАВНОЕ МЕНЮ") {
				t.Fatalf("direct %s did not use shared operation: %s", action, out)
			}
		})
	}
}
func TestRunActionRejectsUnknownBeforeFilesOrUSB(t *testing.T) {
	o, _, n := options(t)
	if err := RunAction(context.Background(), "probe", o); err == nil {
		t.Fatal("unknown action accepted")
	}
	entries, err := os.ReadDir(o.Home)
	if err != nil || len(entries) != 0 || *n != 0 {
		t.Fatal("unknown action had side effects")
	}
}

func TestLocalConnectionTimeoutReturnsToMenu(t *testing.T) {
	o, out, _ := options(t, "", "1", "1", "0")
	o.Open = func(context.Context, time.Duration) (backup.Device, [7]byte, func() error, error) {
		return nil, [7]byte{}, nil, context.DeadlineExceeded
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("local timeout closed menu: %v", err)
	}
	if strings.Count(out.String(), "ГЛАВНОЕ МЕНЮ") != 2 || !strings.Contains(out.String(), "Ошибка:") {
		t.Fatal("no retry after local timeout")
	}
}
