//go:build !windows || !amlogic_mini_driver

package usbdriver

import (
	"context"
	"errors"
)

// New is nil outside Windows release builds; ordinary source builds stay portable.
func New() *Installer                    { return nil }
func HandleCommand([]string) (bool, int) { return false, 0 }
func (*Installer) Ready(context.Context) (bool, error) {
	return false, errors.New("WinUSB preparation is unavailable in this build")
}
func (*Installer) Install(context.Context) error {
	return errors.New("WinUSB preparation is unavailable in this build")
}
