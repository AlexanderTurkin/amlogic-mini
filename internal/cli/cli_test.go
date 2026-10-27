package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestThreeOperationalCommandsDispatchToSharedActions(t *testing.T) {
	for _, name := range []string{"backup", "restore", "flash"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			got := ""
			code := Run(context.Background(), []string{name}, &out, &out, func(_ context.Context, action string) error { got = action; return nil })
			if code != 0 || got != name {
				t.Fatalf("%s: code=%d action=%q", name, code, got)
			}
		})
	}
}
func TestRemovedCommandsAndOldFlagsNeverDispatch(t *testing.T) {
	for _, args := range [][]string{{"probe"}, {"verify", "copy"}, {"menu"}, {"flash", "alpine"}, {"backup", "--mode", "raw"}, {"backup", "--resume"}, {"backup", "--output", "dump"}, {"restore", "--check-device"}, {"flash", "--json"}, {"version", "extra"}, {"--help", "extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			called := false
			code := Run(context.Background(), args, &out, &out, func(context.Context, string) error { called = true; return nil })
			if code != 1 || called {
				t.Fatalf("removed command/flag invoked an operation: %v code=%d", args, code)
			}
		})
	}
}
func TestHelpAndVersionDoNotDispatch(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}, {"version"}, {"--version"}, {"backup", "--help"}, {"restore", "--help"}, {"flash", "--help"}} {
		var out bytes.Buffer
		code := Run(context.Background(), args, &out, &out, func(context.Context, string) error { t.Fatal("help/version started an operation"); return nil })
		if code != 0 || out.Len() == 0 {
			t.Fatalf("%v: %d %s", args, code, out.String())
		}
	}
}
func TestActionErrorsAndCancellationHaveExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{errors.New("read failed"), 1}, {context.Canceled, 130}} {
		var out bytes.Buffer
		code := Run(context.Background(), []string{"backup"}, &out, &out, func(context.Context, string) error { return tc.err })
		if code != tc.code || !strings.Contains(out.String(), tc.err.Error()) {
			t.Fatalf("code=%d error=%s", code, out.String())
		}
	}
}

func TestStatusSanitizesExternalTextAndPreservesExitCode(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"bad\x1b[2J.ubi", "bad?[2J.ubi"},
		{"bad\r\n\u009b2J.ubi", "bad???2J.ubi"},
		{"Образ №1.ubi", "Образ №1.ubi"},
	} {
		var out bytes.Buffer
		err := errors.Join(errors.New(tc.text), context.Canceled)
		if code := Status(&out, err); code != 130 {
			t.Fatalf("exit code: %d", code)
		}
		if got, want := out.String(), "Ошибка: "+tc.want+"?context canceled\n"; got != want {
			t.Fatalf("unsafe error output: %q, want %q", got, want)
		}
	}
}

func TestLicensesAreAvailableWithoutStartingAnOperation(t *testing.T) {
	var out bytes.Buffer
	code := Run(context.Background(), []string{"--licenses"}, &out, &out, func(context.Context, string) error { t.Fatal("licenses started an operation"); return nil })
	if code != 0 || !strings.Contains(out.String(), "GNU LESSER GENERAL PUBLIC LICENSE") || !strings.Contains(out.String(), "libusb") {
		t.Fatalf("license notices missing: code=%d", code)
	}
}
