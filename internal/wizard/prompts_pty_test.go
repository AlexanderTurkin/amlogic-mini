package wizard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"amlogicmini/internal/testutil"
)

func TestTerminalFieldHelper(t *testing.T) {
	mode := os.Getenv("AMLOGIC_MINI_WIZARD_FIELD_CASE")
	if mode == "" {
		return
	}
	title := "Введите путь"
	if mode == "secret" {
		title = "Введите секрет"
	}
	var value string
	err := terminalSession(context.Background(), Options{Input: os.Stdin, Out: os.Stdout}, func(ctx context.Context, o Options) error {
		var askErr error
		value, askErr = o.Prompts.Ask(ctx, Prompt{Title: title, Secret: mode == "secret"})
		return askErr
	})
	if mode == "cancel" {
		if err != nil {
			t.Fatalf("cancel failed: %v", err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		expected := "~/Моё устройство/копия"
		if mode == "secret" {
			expected = "Hidden!  пароль <3"
		}
		if value != expected {
			t.Fatal("field altered the input")
		}
	}
	fmt.Println("FIELD_OK")
}
func TestRealTerminalFieldsMaskPasswordsPreservePathsAndCancel(t *testing.T) {
	for _, term := range []string{"xterm-256color", "dumb"} {
		for _, mode := range []string{"secret", "path", "cancel"} {
			t.Run(term+"/"+mode, func(t *testing.T) {
				c := testutil.StartConsole(t, os.Args[0], []string{"-test.run=^TestTerminalFieldHelper$", "-test.v"}, []string{"AMLOGIC_MINI_WIZARD_FIELD_CASE=" + mode, "TERM=" + term, "NO_COLOR=1"})
				wait := func(token string) { c.WaitFor(t, token) }
				text := c.Text
				title := "Введите путь"
				if mode == "secret" {
					title = "Введите секрет"
				}
				wait(title)
				input := "~/Моё устройство/копия\r"
				if mode == "secret" {
					input = "Hidden!  пароль <3\r"
				}
				if mode == "cancel" {
					input = "\x04"
				}
				c.Write(t, input)
				wait("FIELD_OK")
				if err := c.Wait(t); err != nil {
					t.Fatalf("%v; terminal output: %s", err, text())
				}
				if mode == "secret" && strings.Contains(text(), "Hidden!") {
					t.Fatal("password echoed to terminal")
				}
				if strings.Contains(text(), "\x1b[?1049h") {
					t.Fatal("entered alternate screen")
				}
			})
		}
	}
}
