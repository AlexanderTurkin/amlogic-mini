package wizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"testing"
	"time"

	"amlogicmini/internal/progress"
	"amlogicmini/internal/testutil"
)

func TestInlineTerminalHelper(t *testing.T) {
	mode := os.Getenv("AMLOGIC_MINI_INLINE_CASE")
	if mode == "" {
		return
	}
	opts := Options{Input: os.Stdin, Out: os.Stdout}
	fmt.Println("PREVIOUS_COMMAND_OUTPUT")
	cleaned := false
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := terminalSession(ctx, opts, func(ctx context.Context, o Options) error {
		ui := o.Out.(presentation)
		ui.Screen("Проверка интерфейса")
		if mode == "cancel" {
			ui.Busy("WAIT_FOR_CANCEL")
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			cleaned = true
			return ctx.Err()
		}
		fmt.Fprintln(o.Out, "FIRST_SCREEN")
		if _, err := o.Prompts.Ask(ctx, Prompt{Title: "FIRST_PROMPT"}); err != nil {
			return err
		}
		fmt.Fprintln(o.Out, "SECOND_SCREEN")
		if mode == "secret" {
			value, err := o.Prompts.Ask(ctx, Prompt{Title: "SECRET_PROMPT", Secret: true})
			if err != nil {
				return err
			}
			if value != "Hidden!  пароль <3" {
				return errors.New("wrong secret value")
			}
		}
		for i := 0; i <= 100; i++ {
			ui.Progress(progress.Event{Stage: "write", Item: "SYNTHETIC", Done: int64(i), Total: 100})
			time.Sleep(time.Millisecond)
		}
		ui.Remember("SSH_HINT ssh root@192.168.14.14")
		_, err := o.Prompts.Ask(ctx, Prompt{Title: "FINAL_PROMPT"})
		return err
	})
	if mode == "cancel" {
		if !cleaned || !errors.Is(err, context.Canceled) {
			t.Fatalf("worker not joined: %v %v", cleaned, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	fmt.Println("INLINE_OK")
}

func TestInlineRealTerminal(t *testing.T) {
	for _, term := range []string{"xterm-256color", "dumb"} {
		for _, mode := range []string{"normal", "secret", "cancel"} {
			t.Run(term+"/"+mode, func(t *testing.T) {
				c := testutil.StartConsole(t, os.Args[0], []string{"-test.run=^TestInlineTerminalHelper$", "-test.v"}, []string{"AMLOGIC_MINI_INLINE_CASE=" + mode, "TERM=" + term, "NO_COLOR=1"})
				wait := func(token string) { c.WaitFor(t, token) }
				write := func(text string) { c.Write(t, text) }
				text := c.Text
				if mode == "cancel" {
					wait("WAIT_FOR_CANCEL")
					write("\x03")
				} else {
					wait("FIRST_PROMPT")
					write("\r")
					if mode == "secret" {
						wait("SECRET_PROMPT")
						write("Hidden!  пароль <3\r")
					}
					wait("FINAL_PROMPT")
					c.Resize(t, 60, 18)
					write("\r")
				}
				wait("INLINE_OK")
				if err := c.Wait(t); err != nil {
					t.Fatalf("%v: %s", err, text())
				}
				if runtime.GOOS != "windows" && (strings.Contains(text(), "\x1b[2J") || strings.Contains(text(), "\x1b[3J")) {
					t.Fatal("previous terminal output erased")
				}
				if strings.Contains(text(), "\x1b[?1049h") {
					t.Fatal("alternate screen used")
				}
				if mode == "secret" && strings.Contains(text(), "Hidden!") {
					t.Fatal("password leaked")
				}
				// Windows ConPTY itself emits VT sequences even for plain console output.
				if runtime.GOOS != "windows" && term == "dumb" && strings.Contains(text(), "\x1b[") {
					t.Fatalf("plain terminal has ANSI: %q", text())
				}
				if mode != "cancel" && !strings.Contains(text(), "SSH_HINT ssh root@192.168.14.14") {
					t.Fatal("final SSH hint lost")
				}
			})
		}
	}
}
