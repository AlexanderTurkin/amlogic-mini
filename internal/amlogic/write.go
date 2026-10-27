package amlogic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// WriteLink is deliberately separate from the read-only Link used by backup.
type WriteLink interface {
	Link
	WriteBulk(context.Context, []byte) (int, error)
}

func (c *Client) readWriteStatus(ctx context.Context, b []byte) (int, error) {
	if link, ok := c.link.(interface {
		ReadBulkLong(context.Context, []byte) (int, error)
	}); ok {
		return link.ReadBulkLong(ctx, b)
	}
	return c.link.ReadBulk(ctx, b)
}

// WriteRAM is restricted to the existing scratch buffer, never executable code.
func (c *Client) WriteRAM(ctx context.Context, addr uint32, data []byte) error {
	if len(data) == 0 || addr < 0x01080000 || uint64(addr)+uint64(len(data)) > 0x01900000 {
		return fmt.Errorf("RAM write outside scratch buffer")
	}
	return c.download(ctx, fmt.Sprintf("download mem 0x%x normal 0x%x", addr, len(data)), uint32(len(data)), bytes.NewReader(data), nil)
}

// WritePartition restores logical bytes through the bootloader's named writer.
// Raw NAND, boot chain and individual reserved regions are never accepted.
// acknowledged reports bytes only after device replies; 100% waits for final status.
func (c *Client) WritePartition(ctx context.Context, name string, size uint32, source io.Reader, acknowledged func(uint32)) error {
	allowed := false
	switch name {
	case "boot_a", "boot_b", "system_a", "system_b", "data":
		allowed = true
	}
	valid := false
	for _, r := range c.profile.Partitions {
		if r.Name == name && size > 0 && size <= r.Size && size%c.profile.Geometry.PageSize == 0 {
			valid = true
		}
	}
	if !allowed || !valid || source == nil {
		return fmt.Errorf("partition is protected or write size is invalid: %q", name)
	}
	return c.download(ctx, fmt.Sprintf("download store %s normal 0x%x", name, size), size, source, acknowledged)
}

func (c *Client) download(ctx context.Context, command string, size uint32, source io.Reader, acknowledged func(uint32)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	link, ok := c.link.(WriteLink)
	if !ok {
		return fmt.Errorf("USB transport does not support writes")
	}
	if len(command) > 63 {
		return fmt.Errorf("download command too long")
	}
	// Each vendor operation is bounded; the whole partition may take minutes.
	payload := make([]byte, 64)
	copy(payload, command)
	operation, cancel := context.WithTimeout(ctx, 60*time.Second)
	n, err := link.Control(operation, 0x40, 0x30, 0, 1, payload)
	cancel()
	if err != nil {
		return fmt.Errorf("start download: %w", err)
	}
	if n != 64 {
		return io.ErrShortWrite
	}
	status := make([]byte, 64)
	operation, cancel = context.WithTimeout(ctx, 60*time.Second)
	n, err = link.Control(operation, 0xc0, 0x31, 0, 0, status)
	cancel()
	if err != nil {
		return fmt.Errorf("download initial status: %w", err)
	}
	if n != 64 || string(bytes.TrimRight(status, "\x00\r\n ")) != "success" {
		return fmt.Errorf("download was not accepted")
	}
	chunkBuffer := make([]byte, min(size, uint32(65536)))
	header := make([]byte, 32)
	copy(header[16:], []byte{0xef, 0, 0, 1})
	var sequence uint32
	for remaining := size; remaining > 0; sequence++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := chunkBuffer[:min(remaining, uint32(65536))]
		if _, err := io.ReadFull(source, chunk); err != nil {
			return fmt.Errorf("read download source: %w", err)
		}
		binary.LittleEndian.PutUint32(header[4:], uint32(len(chunk)))
		binary.LittleEndian.PutUint32(header[8:], sequence<<4)
		binary.LittleEndian.PutUint32(header[12:], mediaChecksum(chunk))
		operation, cancel = context.WithTimeout(ctx, 60*time.Second)
		n, err = link.Control(operation, 0x40, 0x32, 1, 0xffff, header)
		if err == nil && n != 32 {
			err = io.ErrShortWrite
		}
		if err == nil {
			n, err = link.WriteBulk(operation, chunk)
			if err == nil && n != len(chunk) {
				err = io.ErrShortWrite
			}
		}
		if err == nil {
			err = c.waitMediaStatus(operation)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("download chunk %d: %w", sequence, err)
		}
		remaining -= uint32(len(chunk))
		if acknowledged != nil && remaining > 0 {
			acknowledged(size - remaining)
		}
	}
	if err := c.commandWithStatus(ctx, "download get_status", 60*time.Second, c.readWriteStatus); err != nil {
		return err
	}
	if acknowledged != nil {
		acknowledged(size)
	}
	return nil
}

func (c *Client) waitMediaStatus(ctx context.Context) error {
	for {
		reply := make([]byte, 512)
		n, err := c.readWriteStatus(ctx, reply)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(reply) {
			return fmt.Errorf("empty or invalid write status")
		}
		status := string(bytes.TrimRight(reply[:n], "\x00\r\n "))
		if strings.HasPrefix(status, "OK!!") {
			return nil
		}
		if !strings.HasPrefix(status, "Continue:32") {
			return fmt.Errorf("bootloader rejected media write: %q", status[:min(len(status), 96)])
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mediaChecksum(data []byte) uint32 {
	var sum uint32
	for len(data) >= 4 {
		sum += binary.LittleEndian.Uint32(data)
		data = data[4:]
	}
	var tail [4]byte
	copy(tail[:], data)
	return sum + binary.LittleEndian.Uint32(tail[:])
}

// StockBoot is called only after the restore orchestrator verifies every file.
func (c *Client) StockBoot(ctx context.Context) error {
	for _, command := range []string{"echo; setenv bootcmd 'run storeboot'", "echo; saveenv"} {
		if err := c.commandWithStatus(ctx, command, 60*time.Second, c.readWriteStatus); err != nil {
			return err
		}
	}
	env, err := c.Environment(ctx)
	if err != nil {
		return fmt.Errorf("read stock boot environment: %w", err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(env), "\x00"), "\n") {
		if line == "bootcmd=run storeboot" {
			return nil
		}
	}
	return fmt.Errorf("stock bootcmd readback did not match")
}
