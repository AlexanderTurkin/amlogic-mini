// Package amlogic implements typed A113X bootloader operations.
package amlogic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"amlogicmini/internal/profile"
)

type Link interface {
	Control(context.Context, byte, byte, uint16, uint16, []byte) (int, error)
	ReadBulk(context.Context, []byte) (int, error)
}

type Client struct {
	link    Link
	profile profile.Profile
}

func New(link Link, p profile.Profile) *Client { return &Client{link: link, profile: p} }

func (c *Client) Identify(ctx context.Context) ([7]byte, error) {
	var version [7]byte
	if err := ctx.Err(); err != nil {
		return version, err
	}
	n, err := c.link.Control(ctx, 0xc0, 0x20, 0, 0, version[:])
	if err != nil {
		return version, fmt.Errorf("identify: %w", err)
	}
	if n != 7 || bytes.Equal(version[:4], []byte{0, 0, 0, 0}) {
		return version, fmt.Errorf("invalid identify response (%d bytes)", n)
	}
	return version, nil
}

func (c *Client) command(ctx context.Context, command string) error {
	return c.commandWithStatus(ctx, command, 30*time.Second, c.link.ReadBulk)
}

func (c *Client) commandWithStatus(ctx context.Context, command string, timeout time.Duration, read func(context.Context, []byte) (int, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if len(command) > 63 || stringsContainNUL(command) {
		return fmt.Errorf("bootloader command exceeds payload limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload := make([]byte, 64)
	copy(payload, command)
	n, err := c.link.Control(ctx, 0x40, 0x34, 0, 2, payload)
	if err != nil {
		return fmt.Errorf("send read command: %w", err)
	}
	if n != 64 {
		return io.ErrShortWrite
	}
	for {
		response := make([]byte, 512)
		n, err = read(ctx, response)
		if err != nil {
			return fmt.Errorf("read command status: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("empty command status")
		}
		status := bytes.TrimRight(response[:n], "\x00\r\n ")
		if bytes.HasPrefix(status, []byte("Continue:34")) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
				continue
			}
		}
		if !bytes.HasPrefix(status, []byte("success")) {
			return fmt.Errorf("bootloader rejected read command")
		}
		return nil
	}
}

func stringsContainNUL(s string) bool             { return bytes.IndexByte([]byte(s), 0) >= 0 }
func (c *Client) Prime(ctx context.Context) error { return c.command(ctx, "echo priming") }

func (c *Client) receive(ctx context.Context, size uint32, out io.Writer) (int64, error) {
	var total int64
	buf := make([]byte, min(size, uint32(65536)))
	metadata := make([]byte, 16)
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		next := min(remaining, uint32(65536))
		n, err := c.link.Control(ctx, 0xc0, 0x33, 4096, uint16((next+4095)/4096), metadata)
		if err != nil {
			return total, fmt.Errorf("request media data: %w", err)
		}
		if n != 16 {
			return total, fmt.Errorf("short media metadata: %d bytes", n)
		}
		for received := uint32(0); received < next; {
			part := buf[:next-received]
			n, readErr := c.link.ReadBulk(ctx, part)
			if n < 0 || n > len(part) {
				return total, fmt.Errorf("invalid USB transfer length %d", n)
			}
			if n > 0 {
				written, writeErr := out.Write(part[:n])
				total += int64(written)
				if writeErr != nil {
					return total, fmt.Errorf("save media data: %w", writeErr)
				}
				if written != n {
					return total, io.ErrShortWrite
				}
				received += uint32(n)
				remaining -= uint32(n)
			}
			if readErr != nil {
				return total, fmt.Errorf("receive media at byte %d: %w", total, readErr)
			}
			if n == 0 {
				return total, io.ErrUnexpectedEOF
			}
		}
	}
	return total, nil
}

func (c *Client) ReadRAM(ctx context.Context, addr, size uint32) ([]byte, error) {
	end := uint64(addr) + uint64(size)
	valid := size > 0 && ((addr >= 0x01080000 && end <= 0x01900000) || (addr >= 0x02000000 && end <= 0x02010000))
	if !valid {
		return nil, fmt.Errorf("RAM read outside backup buffers")
	}
	if err := c.command(ctx, fmt.Sprintf("upload mem 0x%x normal 0x%x", addr, size)); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(int(size))
	_, err := c.receive(ctx, size, &out)
	return out.Bytes(), err
}

func (c *Client) ReadPartition(ctx context.Context, name string, size uint32, out io.Writer) (int64, error) {
	valid := false
	for _, r := range c.profile.Partitions {
		if r.Name == name && size > 0 && size <= r.Size {
			valid = true
			break
		}
	}
	if !valid {
		return 0, fmt.Errorf("unknown partition or invalid size: %q", name)
	}
	if err := c.command(ctx, fmt.Sprintf("upload store %s normal 0x%x", name, size)); err != nil {
		return 0, err
	}
	return c.receive(ctx, size, out)
}

func (c *Client) ReadNAND(ctx context.Context, offset, size uint32, raw bool) ([]byte, error) {
	g := c.profile.Geometry
	if size == 0 || size > g.ChunkSize || uint64(offset)+uint64(size) > uint64(g.NANDSize) {
		return nil, fmt.Errorf("NAND read outside profile")
	}
	count := size
	operation := "read"
	if raw {
		if offset%g.PageSize != 0 || size%g.PageSize != 0 {
			return nil, fmt.Errorf("unaligned raw NAND read")
		}
		operation = "read.raw"
		count = size / g.PageSize
	}
	readSize := size
	if raw {
		readSize = g.RawBytes(size)
	}
	// A failed NAND command must not return stale contents from an earlier read.
	if err := c.command(ctx, fmt.Sprintf("echo; mw.b 0x01080000 0xa5 0x%x", readSize)); err != nil {
		return nil, err
	}
	if err := c.command(ctx, fmt.Sprintf("echo; nand %s 0x01080000 0x%x 0x%x", operation, offset, count)); err != nil {
		return nil, err
	}
	data, err := c.ReadRAM(ctx, 0x01080000, readSize)
	if err == nil && bytes.Count(data, []byte{0xa5}) == len(data) {
		return nil, errors.New("NAND did not replace the RAM read buffer")
	}
	return data, err
}

func (c *Client) Environment(ctx context.Context) ([]byte, error) {
	for _, command := range []string{"echo; mw.b 0x02000000 0 0x10000", "echo; env export -t 0x02000000"} {
		if err := c.command(ctx, command); err != nil {
			return nil, err
		}
	}
	return c.ReadRAM(ctx, 0x02000000, 0x10000)
}
