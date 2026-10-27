package smoke

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"amlogicmini/internal/testutil"
)

func executable(t *testing.T) string {
	t.Helper()
	source := os.Getenv("AMLOGIC_MINI_TEST_EXE")
	if source == "" {
		t.Skip("set AMLOGIC_MINI_TEST_EXE to the packaged executable")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Моё устройство с пробелами")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "amlogic-mini" + filepath.Ext(source)
	target := filepath.Join(dir, name)
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		in.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	if err = errors.Join(copyErr, in.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestPackagedCommands(t *testing.T) {
	binary := executable(t)
	for _, tc := range []struct {
		arg, text string
		exit      int
	}{{"version", "Amlogic A113X Mini", 0}, {"--help", "Команды:", 0}, {"--licenses", "GNU LESSER GENERAL PUBLIC LICENSE", 0}, {"probe", "неизвестная команда", 1}} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, binary, tc.arg).CombinedOutput()
		contextErr := ctx.Err()
		cancel()
		if contextErr != nil {
			t.Fatalf("%s timed out: %v; output %s", tc.arg, contextErr, out)
		}
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != tc.exit || !strings.Contains(string(out), tc.text) {
			t.Fatalf("%s: exit %d output %s", tc.arg, code, out)
		}
	}
}

func TestPackagedMenuValidationAndExit(t *testing.T) {
	for _, term := range []string{"xterm-256color", "dumb"} {
		t.Run(term, func(t *testing.T) {
			binary := executable(t)
			c := testutil.StartConsole(t, binary, nil, []string{"TERM=" + term, "NO_COLOR=1"})
			c.WaitFor(t, "CLI-инструмент для прошивки Alpine Linux.")
			c.WaitFor(t, "Нажмите Enter")
			c.Write(t, "\r")
			c.WaitFor(t, "ГЛАВНОЕ МЕНЮ")
			c.WaitFor(t, "Ваш выбор")
			c.Write(t, "9\r")
			c.WaitFor(t, "введите цифру от 0 до 3")
			c.Resize(t, 60, 18)
			c.Write(t, "\x150\r")
			if err := c.Wait(t); err != nil {
				t.Fatalf("menu exit: %v; %s", err, c.Text())
			}
			if strings.Contains(c.Text(), "\x1b[?1049h") {
				t.Fatal("alternate screen used")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "backups")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("menu touched backups")
			}
		})
	}
}

func TestPackagedActionsDefaultToCancellation(t *testing.T) {
	for _, action := range []string{"backup", "restore", "flash"} {
		t.Run(action, func(t *testing.T) {
			binary := executable(t)
			c := testutil.StartConsole(t, binary, []string{action}, []string{"TERM=xterm-256color", "NO_COLOR=1"})
			if action == "restore" {
				c.WaitFor(t, "нет резервных копий")
				c.WaitFor(t, "Нажмите Enter")
			} else {
				c.WaitFor(t, "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?")
				c.WaitFor(t, "Ваш выбор")
			}
			c.Write(t, "\r")
			if err := c.Wait(t); err != nil {
				t.Fatalf("cancel %s: %v; %s", action, err, c.Text())
			}
			if strings.Contains(c.Text(), "Ждём подключения устройства") {
				t.Fatal("default cancellation opened USB")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "backups")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("canceled action created a backup directory")
			}
		})
	}
}

func TestPackagedCtrlC(t *testing.T) {
	binary := executable(t)
	c := testutil.StartConsole(t, binary, nil, []string{"TERM=xterm-256color", "NO_COLOR=1"})
	c.WaitFor(t, "Нажмите Enter")
	c.Write(t, "\x03")
	var exit *exec.ExitError
	if err := c.Wait(t); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("cancellation exit: %v; %s", err, c.Text())
	}
}

// Opt in only on a Windows host without a configured device. This checks the
// real packaged driver prompt, but refuses installation and never opens USB.
func TestPackagedWindowsDriverDecline(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AMLOGIC_MINI_TEST_DRIVER") != "1" {
		t.Skip("requires explicit driver-prompt check on an unconfigured Windows host")
	}
	binary := executable(t)
	c := testutil.StartConsole(t, binary, []string{"backup"}, []string{"TERM=xterm-256color", "NO_COLOR=1"})
	c.WaitFor(t, "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?")
	c.WaitFor(t, "Ваш выбор")
	c.Write(t, "1\r")
	c.WaitFor(t, "НАСТРОИТЬ USB ДЛЯ УСТРОЙСТВА?")
	c.WaitFor(t, "сертификат")
	c.Write(t, "\r")
	c.WaitFor(t, "настройка USB отменена")
	c.WaitFor(t, "Нажмите Enter")
	c.Write(t, "\r")
	var exit *exec.ExitError
	if err := c.Wait(t); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("decline exit: %v; %s", err, c.Text())
	}
	if strings.Contains(c.Text(), "Ждём подключения устройства") {
		t.Fatal("declined driver setup opened USB")
	}
}

// Explicit opt-in: prepares only the fixed WinUSB package on a test Windows VM.
// No device may be connected. Stop at USB waiting, before any device operation.
func TestPackagedWindowsDriverPreparation(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AMLOGIC_MINI_TEST_DRIVER_INSTALL") != "1" {
		t.Skip("requires explicit consent to prepare WinUSB on a test Windows VM")
	}
	binary := executable(t)
	c := testutil.StartConsoleFor(t, 120*time.Second, binary, []string{"backup"}, []string{"TERM=xterm-256color", "NO_COLOR=1"})
	c.WaitFor(t, "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?")
	c.WaitFor(t, "Ваш выбор")
	c.Write(t, "1\r")
	c.WaitFor(t, "НАСТРОИТЬ USB ДЛЯ УСТРОЙСТВА?")
	c.WaitFor(t, "сертификат")
	c.Write(t, "1\r")
	c.WaitFor(t, "Ждём подключения устройства")
	c.Write(t, "\x03")
	var exit *exec.ExitError
	if err := c.Wait(t); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("prepared driver exit: %v; %s", err, c.Text())
	}
	// A staged package must work again without another certificate/UAC cycle.
	again := testutil.StartConsole(t, binary, []string{"backup"}, []string{"TERM=xterm-256color", "NO_COLOR=1"})
	again.WaitFor(t, "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?")
	again.WaitFor(t, "Ваш выбор")
	again.Write(t, "1\r")
	again.WaitFor(t, "Ждём подключения устройства")
	if strings.Contains(again.Text(), "НАСТРОИТЬ USB ДЛЯ УСТРОЙСТВА?") {
		t.Fatal("prepared driver was not recognized")
	}
	again.Write(t, "\x03")
	if err := again.Wait(t); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("prepared driver reuse: %v; %s", err, again.Text())
	}
}
