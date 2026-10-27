// Package progress describes measured transfer stages without terminal formatting.
package progress

import "io"

type Event struct {
	Stage       string
	Item        string
	Done, Total int64 // Total=0 means the amount of work is unknown.
}
type Notify func(Event)

// Writer counts only bytes accepted by the underlying writer and preserves its errors.
func Writer(out io.Writer, event Event, notify Notify) io.Writer {
	if notify == nil {
		return out
	}
	notify(event)
	return &counter{out: out, event: event, notify: notify}
}

type counter struct {
	out    io.Writer
	event  Event
	notify Notify
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.out.Write(p)
	c.event.Done += int64(n)
	c.notify(c.event)
	return n, err
}
