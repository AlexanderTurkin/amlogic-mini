package usbdriver

import (
	"context"
	"testing"
)

func TestOnlyA113XHardwareIDMatches(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{`USB\VID_1B8E&PID_C003`, true},
		{`usb\vid_1b8e&pid_c003`, true},
		{`USB\VID_1B8E&PID_C0030`, false},
		{`USB\VID_1B8E&PID_C003&MI_00`, false},
		{`USB\VID_1B8E&PID_C004`, false},
		{`USB\VID_01B8&PID_C003`, false},
		{`USB\MS_COMP_WINUSB`, false},
		{"", false},
	} {
		if got := isTarget(tc.id); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestDriverReadinessIncludesStagedPackagesAndIgnoresStaleNodes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		present, previous binding
		staged, want      bool
	}{
		{name: "bound present", present: binding{found: true, ready: true}, want: true},
		{name: "present wrong driver", present: binding{found: true}, staged: true},
		{name: "preinstalled without device", staged: true, want: true},
		{name: "historical WinUSB", previous: binding{found: true, ready: true}, want: true},
		{name: "unbound historical device", previous: binding{found: true}},
		{name: "never configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkReady(context.Background(), func(_ context.Context, present bool) (binding, error) {
				if present {
					return tc.present, nil
				}
				return tc.previous, nil
			}, func() (bool, error) { return tc.staged, nil })
			if err != nil || got != tc.want {
				t.Fatalf("ready=%v error=%v, want %v", got, err, tc.want)
			}
		})
	}
}
