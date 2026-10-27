package main

import (
	"context"
	"os"
	"os/signal"
	"time"

	"github.com/charmbracelet/x/term"

	"amlogicmini/internal/amlogic"
	"amlogicmini/internal/backup"
	"amlogicmini/internal/cli"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/usb"
	"amlogicmini/internal/usbdriver"
	"amlogicmini/internal/wizard"
)

func main() { os.Exit(run()) }
func run() int {
	if handled, code := usbdriver.HandleCommand(os.Args[1:]); handled {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	open := func(ctx context.Context, timeout time.Duration) (backup.Device, [7]byte, func() error, error) {
		connection, version, err := usb.Open(ctx, timeout)
		if err != nil {
			return nil, version, nil, err
		}
		return amlogic.New(connection, profile.A113X()), version, connection.Close, nil
	}
	args := os.Args[1:]
	options := wizard.Options{Out: os.Stdout, Input: os.Stdin, Open: open, Version: cli.Version}
	if driver := usbdriver.New(); driver != nil {
		options.USBDriver = driver
	}
	if len(args) == 0 && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) {
		return cli.Status(os.Stderr, wizard.Run(ctx, options))
	}
	return cli.Run(ctx, args, os.Stdout, os.Stderr, func(ctx context.Context, action string) error { return wizard.RunAction(ctx, action, options) })
}
