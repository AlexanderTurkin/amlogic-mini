package usb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/gousb"
)

func TestNativeCancellationPreservesCauseAndPartialCount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := transferResult(ctx, 17, gousb.TransferCancelled)
	if n != 17 || !errors.Is(err, context.Canceled) {
		t.Fatalf("native cancellation became %d %v", n, err)
	}
}

type bulkInputFunc func(context.Context, []byte) (int, error)

func (f bulkInputFunc) ReadContext(ctx context.Context, b []byte) (int, error) { return f(ctx, b) }

func TestNativeBulkAlignmentAndPartialResults(t *testing.T) {
	for _, size := range []int{3, 512, 65536} {
		b := make([]byte, size)
		c := &Connection{ep: bulkInputFunc(func(_ context.Context, packet []byte) (int, error) {
			if len(packet)%512 != 0 || len(packet) < size {
				t.Fatalf("invalid packet buffer: %d", len(packet))
			}
			if size%512 == 0 && &packet[0] != &b[0] {
				t.Fatal("aligned read allocated a second buffer")
			}
			copy(packet, bytes.Repeat([]byte{0x35}, size))
			return size, nil
		})}
		n, err := c.readBulk(context.Background(), b, time.Second)
		if err != nil || n != size || bytes.Count(b, []byte{0x35}) != size {
			t.Fatalf("read %d: %d %v", size, n, err)
		}
	}
	b := make([]byte, 3)
	c := &Connection{ep: bulkInputFunc(func(_ context.Context, packet []byte) (int, error) { copy(packet, "abcd"); return 4, nil })}
	if n, err := c.readBulk(context.Background(), b, time.Second); err == nil || n != 0 || !bytes.Equal(b, make([]byte, 3)) {
		t.Fatalf("overrun accepted: %d %v", n, err)
	}
	c.ep = bulkInputFunc(func(_ context.Context, packet []byte) (int, error) { copy(packet, "ab"); return 2, io.ErrUnexpectedEOF })
	if n, err := c.readBulk(context.Background(), b, time.Second); n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || string(b[:n]) != "ab" {
		t.Fatalf("lost partial result: %d %v", n, err)
	}
}
