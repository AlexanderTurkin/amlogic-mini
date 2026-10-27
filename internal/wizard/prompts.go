package wizard

import (
	"context"
	"errors"
	"os"

	"charm.land/bubbles/v2/textinput"
)

// ErrInputClosed is reserved for user cancellation, never source/transport EOF.
var ErrInputClosed = errors.New("ввод отменён")

type Prompt struct {
	Title    string
	Secret   bool
	Validate func(string) error
}
type Prompts interface {
	Ask(context.Context, Prompt) (string, error)
}

func inputField(prompt Prompt, width int) *textinput.Model {
	field := textinput.New()
	field.Prompt = "> "
	field.CharLimit = 4096
	field.SetWidth(width)
	if prompt.Secret {
		field.EchoMode = textinput.EchoNone
	}
	return &field
}
func colorAllowed() bool { return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" }
