package usb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/gousb"

	"amlogicmini/internal/amlogic"
	"amlogicmini/internal/profile"
)

type bulkInput interface {
	ReadContext(context.Context, []byte) (int, error)
}

type Connection struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface
	ep   bulkInput
	out  *gousb.OutEndpoint
}

func Open(ctx context.Context, timeout time.Duration) (*Connection, [7]byte, error) {
	var version [7]byte
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	usbctx := gousb.NewContext()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			usbctx.Close()
			return nil, version, fmt.Errorf("wait for Amlogic USB (last error: %v): %w", lastErr, err)
		}
		devices, err := usbctx.OpenDevices(func(d *gousb.DeviceDesc) bool { return d.Vendor == 0x1b8e && d.Product == 0xc003 })
		if err != nil {
			lastErr = err
		}
		if len(devices) > 1 {
			for _, d := range devices {
				d.Close()
			}
			usbctx.Close()
			return nil, version, fmt.Errorf("multiple Amlogic devices; connect only one device")
		}
		if len(devices) == 1 {
			c := &Connection{ctx: usbctx, dev: devices[0]}
			version, err = amlogic.New(c, profile.A113X()).Identify(ctx)
			if err == nil {
				c.cfg, err = c.dev.Config(1)
				if err == nil {
					c.intf, err = c.cfg.Interface(0, 0)
				}
				if err == nil {
					c.ep, err = c.intf.InEndpoint(1)
				}
				if err != nil {
					c.Close()
					return nil, version, fmt.Errorf("claim USB interface (check permissions/WinUSB driver): %w", err)
				}
				return c, version, nil
			}
			lastErr = err
			c.dev.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *Connection) Control(ctx context.Context, k, r byte, v, i uint16, b []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	timeout := 500 * time.Millisecond
	if r == 0x30 || r == 0x31 || r == 0x32 {
		timeout = 60 * time.Second
	}
	if r == 0x20 {
		// The vendor identify ioctl waits 5000 ms. A short timeout makes cold
		// or slow boards look like they are not present.
		timeout = 5 * time.Second
	}
	if end, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(end))
		if timeout <= 0 {
			return 0, context.DeadlineExceeded
		}
	}
	c.dev.ControlTimeout = max(timeout, time.Millisecond)
	n, err := c.dev.Control(k, r, v, i, b)
	return transferResult(ctx, n, err)
}

func (c *Connection) ReadBulk(ctx context.Context, b []byte) (int, error) {
	return c.readBulk(ctx, b, 5*time.Second)
}

func (c *Connection) ReadBulkLong(ctx context.Context, b []byte) (int, error) {
	return c.readBulk(ctx, b, 60*time.Second)
}

func (c *Connection) readBulk(ctx context.Context, b []byte, timeout time.Duration) (int, error) {
	if c.ep == nil {
		return 0, fmt.Errorf("USB bulk endpoint not claimed")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// libusb requires packet-aligned buffers even when the protocol expects a short tail.
	size := (len(b) + 511) / 512 * 512
	buf := b
	if size != len(b) {
		buf = make([]byte, size)
	}
	n, err := c.ep.ReadContext(ctx, buf)
	if n > len(b) {
		return 0, fmt.Errorf("USB returned %d bytes, expected at most %d", n, len(b))
	}
	if size != len(b) {
		copy(b, buf[:n])
	}
	return transferResult(ctx, n, err)
}

// libusb reports TransferCancelled rather than context.Canceled. Preserve the
// context cause so Ctrl+C has the same result during every protocol phase.
func transferResult(ctx context.Context, n int, err error) (int, error) {
	if cause := ctx.Err(); cause != nil {
		return n, cause
	}
	return n, err
}

func (c *Connection) Close() error {
	var errs []error
	if c.intf != nil {
		c.intf.Close()
		c.intf = nil
	}
	if c.cfg != nil {
		errs = append(errs, c.cfg.Close())
		c.cfg = nil
	}
	if c.dev != nil {
		errs = append(errs, c.dev.Close())
		c.dev = nil
	}
	if c.ctx != nil {
		errs = append(errs, c.ctx.Close())
		c.ctx = nil
	}
	return errors.Join(errs...)
}

func (c *Connection) WriteBulk(ctx context.Context, b []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if c.intf == nil {
		return 0, fmt.Errorf("USB interface not claimed")
	}
	if c.out == nil {
		var err error
		c.out, err = c.intf.OutEndpoint(2)
		if err != nil {
			return 0, fmt.Errorf("open USB OUT endpoint: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := c.out.WriteContext(ctx, b)
	return transferResult(ctx, n, err)
}
