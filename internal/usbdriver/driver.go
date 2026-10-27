// Package usbdriver prepares the Windows WinUSB binding for the fixed bootloader ID.
package usbdriver

import (
	"context"
	"strings"
)

type Installer struct{}

func isTarget(id string) bool {
	return strings.EqualFold(id, `USB\VID_1B8E&PID_C003`)
}

type binding struct{ found, ready bool }

func checkReady(ctx context.Context, lookup func(context.Context, bool) (binding, error), staged func() (bool, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := lookup(ctx, true)
	if err != nil || current.found {
		return current.ready, err
	}
	prepared, err := staged()
	if err != nil || prepared {
		return prepared, err
	}
	previous, err := lookup(ctx, false)
	return previous.ready, err
}
