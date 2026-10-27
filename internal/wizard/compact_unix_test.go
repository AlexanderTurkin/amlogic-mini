//go:build !windows

package wizard

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"
)

func TestCompactWelcomeThroughTerminalBridge(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 18, Cols: 40}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	bridge := &terminalBridge{send: func(msg tea.Msg) {
		if line, ok := msg.(lineMsg); ok {
			text.WriteString(string(line))
		}
	}}
	w := newWizard(Options{Input: slave, Out: bridge})
	w.welcome()
	if strings.Contains(text.String(), "+----------------------+") {
		t.Fatal("compact welcome still emits the full-size drawing through terminalBridge")
	}
}
