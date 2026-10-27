package testutil

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/xpty"
)

// Console drives a real Unix PTY or Windows ConPTY without shell input emulation.
type Console struct {
	pty              xpty.Pty
	cmd              *exec.Cmd
	ctx              context.Context
	exited, readDone chan struct{}
	err              error
	mu               sync.Mutex
	output           strings.Builder
}

func StartConsole(t testing.TB, executable string, args, env []string) *Console {
	t.Helper()
	return StartConsoleFor(t, 25*time.Second, executable, args, env)
}

// StartConsoleFor allows explicitly opted-in installer checks to wait for Windows.
func StartConsoleFor(t testing.TB, timeout time.Duration, executable string, args, env []string) *Console {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	p, err := xpty.NewPty(80, 24)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &Console{pty: p, ctx: ctx, exited: make(chan struct{}), readDone: make(chan struct{})}
	c.cmd = exec.CommandContext(ctx, executable, args...)
	c.cmd.SysProcAttr = consoleProcessAttrs()
	c.cmd.Env = append(os.Environ(), env...)
	if err = p.Start(c.cmd); err != nil {
		p.Close()
		cancel()
		t.Fatal(err)
	}
	go func() {
		defer close(c.readDone)
		buf := make([]byte, 8192)
		for {
			n, e := p.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.output.Write(buf[:n])
				c.mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	go func() { c.err = xpty.WaitProcess(ctx, c.cmd); close(c.exited) }()
	t.Cleanup(func() {
		cancel()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = p.Close()
		<-c.exited
		select {
		case <-c.readDone:
		case <-time.After(time.Second):
			t.Error("console reader did not stop")
		}
	})
	return c
}
func (c *Console) Text() string { c.mu.Lock(); defer c.mu.Unlock(); return c.output.String() }
func (c *Console) WaitFor(t testing.TB, text string) {
	t.Helper()
	for !strings.Contains(ansi.Strip(c.Text()), text) {
		select {
		case <-c.ctx.Done():
			t.Fatalf("missing %q: %s", text, c.Text())
		case <-c.exited:
			_ = c.Wait(t)
			if strings.Contains(ansi.Strip(c.Text()), text) {
				return
			}
			t.Fatalf("console exited before %q: %v; %s", text, c.err, c.Text())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (c *Console) Write(t testing.TB, text string) {
	t.Helper()
	if _, err := io.WriteString(c.pty, text); err != nil {
		t.Fatal(err)
	}
}
func (c *Console) Resize(t testing.TB, width, height int) {
	t.Helper()
	if err := c.pty.Resize(width, height); err != nil {
		t.Fatal(err)
	}
}
func (c *Console) Wait(t testing.TB) error {
	t.Helper()
	<-c.exited
	_ = c.pty.Close()
	select {
	case <-c.readDone:
	case <-time.After(time.Second):
		t.Fatal("console output did not close")
	}
	return c.err
}
