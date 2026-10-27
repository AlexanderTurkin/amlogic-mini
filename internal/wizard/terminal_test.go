package wizard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"amlogicmini/internal/progress"
)

func TestInlineModelReplacesScreenAndProgress(t *testing.T) {
	m := newTerminalModel(func() {}, nil)
	m.Update(screenMsg("Старый экран"))
	m.Update(lineMsg("OLD_CONTENT"))
	m.Update(screenMsg("Прошивка"))
	for i := 0; i <= 100; i++ {
		m.Update(progressMsg{Event: progress.Event{Stage: "write", Item: "kernel", Done: int64(i), Total: 100}})
	}
	view := m.View()
	if view.AltScreen || strings.Contains(view.Content, "OLD_CONTENT") || strings.Count(view.Content, "100%") != 1 || strings.Contains(view.Content, "99%") {
		t.Fatalf("not one replaceable block: %s", view.Content)
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	for _, line := range strings.Split(m.View().Content, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Fatalf("line too wide: %q", line)
		}
	}
}
func TestWaitingScreenNamesTheTask(t *testing.T) {
	m := newTerminalModel(func() {}, nil)
	m.Update(progressMsg{Event: progress.Event{Stage: "Ждём подключения устройства (до 2 минут)"}, keepText: true})
	view := m.View().Content
	if strings.Contains(view, "Пожалуйста, подождите") || strings.Count(view, "Ждём подключения устройства") != 1 {
		t.Fatalf("waiting screen does not explain the task: %s", view)
	}
}

func TestInlineCancellationWaitsForWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newTerminalModel(cancel, nil)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if ctx.Err() == nil || cmd != nil {
		t.Fatal("cancel must stop worker but keep renderer alive")
	}
	_, cmd = m.Update(workerDoneMsg{err: ctx.Err()})
	if cmd == nil {
		t.Fatal("renderer did not exit after worker cleanup")
	}
}
func TestInlineSecretNeverEntersRenderedContent(t *testing.T) {
	m := newTerminalModel(func() {}, nil)
	request := promptMsg{prompt: Prompt{Title: "Пароль", Secret: true}, reply: make(chan promptReply, 1)}
	m.Update(request)
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "PrivatePassword"})
	if strings.Contains(m.View().Content, "PrivatePassword") {
		t.Fatal("password leaked")
	}
}

func TestInlineNoColorBarHasNoANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newTerminalModel(func() {}, nil)
	m.Update(progressMsg{Event: progress.Event{Stage: "write", Done: 1, Total: 2}})
	if strings.Contains(m.View().Content, "\x1b[") {
		t.Fatal("NO_COLOR emitted styling")
	}
}

// Drive asynchronous child commands deterministically without a terminal.
func childMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, childMessages(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}
func TestStaleFieldEventsCannotAffectNewPrompt(t *testing.T) {
	m := newTerminalModel(func() {}, nil)
	first := promptMsg{prompt: Prompt{Title: "first"}, reply: make(chan promptReply, 1)}
	m.Update(first)
	oldID := m.fieldID
	stale := fieldCommand(oldID, func() tea.Msg {
		return tea.BatchMsg{func() tea.Msg { return tea.KeyPressMsg{Code: tea.KeyEnter} }, func() tea.Msg { return tea.KeyPressMsg{Text: "old secret"} }}
	})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	second := promptMsg{prompt: Prompt{Title: "second", Validate: func(string) error { return errors.New("input required") }}, reply: make(chan promptReply, 1)}
	m.Update(second)
	for _, msg := range childMessages(stale) {
		m.Update(msg)
	}
	if m.request == nil || m.input.Value() != "" {
		t.Fatal("stale command affected next input")
	}
	select {
	case <-second.reply:
		t.Fatal("stale command submitted next prompt")
	default:
	}
}

func TestInputValidationDoesNotSubmitUntilCorrected(t *testing.T) {
	m := newTerminalModel(func() {}, nil)
	request := promptMsg{prompt: Prompt{Title: "confirmation", Validate: func(value string) error {
		if value != "1" {
			return errors.New("введите 1")
		}
		return nil
	}}, reply: make(chan promptReply, 1)}
	m.Update(request)
	m.Update(tea.KeyPressMsg{Text: "9"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case <-request.reply:
		t.Fatal("invalid confirmation submitted")
	default:
	}
	if !strings.Contains(m.View().Content, "введите 1") {
		t.Fatal("validation error not visible")
	}
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m.Update(tea.KeyPressMsg{Text: "1"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case reply := <-request.reply:
		if reply.err != nil || reply.value != "1" {
			t.Fatalf("wrong result: %+v", reply)
		}
	default:
		t.Fatal("valid input was not submitted")
	}
}

type terminalSpy struct {
	*bytes.Buffer
	summary string
}

func (s *terminalSpy) Screen(string)           {}
func (s *terminalSpy) Busy(string)             {}
func (s *terminalSpy) Progress(progress.Event) {}
func (s *terminalSpy) Remember(v string)       { s.summary = v }
func TestEmptyRestoreWaitsForAcknowledgement(t *testing.T) {
	o, _, _ := options(t, "", "2", "", "0")
	spy := &terminalSpy{Buffer: new(bytes.Buffer)}
	o.Out = spy
	prompts := &ackPrompts{script: &script{values: []string{"", "2", "", "0"}}, out: spy}
	o.Prompts = prompts
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !prompts.acknowledged {
		t.Fatal("missing-copy message vanished before acknowledgement")
	}
}

type ackPrompts struct {
	*script
	out          *terminalSpy
	acknowledged bool
}

func (p *ackPrompts) Ask(ctx context.Context, prompt Prompt) (string, error) {
	if strings.Contains(prompt.Title, "Нажмите Enter") && strings.Contains(p.out.String(), "нет резервных копий") {
		p.acknowledged = true
	}
	return p.script.Ask(ctx, prompt)
}
func TestPostFlashCancellationRetainsSSHHint(t *testing.T) {
	o, _, _ := options(t)
	spy := &terminalSpy{Buffer: new(bytes.Buffer)}
	o.Out = spy
	if err := newWizard(o).setup(context.Background()); !errors.Is(err, ErrInputClosed) {
		t.Fatal(err)
	}
	if !strings.Contains(spy.summary, "ssh root@192.168.14.14") {
		t.Fatal("SSH command lost on cancellation")
	}
}
