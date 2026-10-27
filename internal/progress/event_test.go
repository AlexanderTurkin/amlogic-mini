package progress

import (
	"errors"
	"io"
	"testing"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, io.ErrShortWrite }
func TestWriterReportsOnlySuccessfulBytesAndPreservesError(t *testing.T) {
	var events []Event
	writer := Writer(shortWriter{}, Event{Stage: "write", Item: "data", Total: 8}, func(e Event) { events = append(events, e) })
	n, err := writer.Write([]byte("abcdefgh"))
	if n != 4 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write changed: %d %v", n, err)
	}
	if len(events) != 2 || events[0].Done != 0 || events[1].Done != 4 || events[1].Total != 8 {
		t.Fatalf("inaccurate progress: %+v", events)
	}
}
