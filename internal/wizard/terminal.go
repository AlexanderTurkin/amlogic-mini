package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	bar "charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"amlogicmini/internal/appinfo"
	"amlogicmini/internal/progress"
)

type presentation interface {
	Screen(string)
	Progress(progress.Event)
	Busy(string)
	Remember(string)
}
type screenMsg string
type lineMsg string
type summaryMsg string
type progressMsg struct {
	progress.Event
	keepText bool
}
type promptReply struct {
	value string
	err   error
}
type promptMsg struct {
	prompt Prompt
	reply  chan promptReply
}
type workerDoneMsg struct{ err error }
type cancelMsg struct{}
type inputClosedMsg struct{}
type fieldMsg struct {
	id      uint64
	message tea.Msg
}

// Scope every asynchronous child result, including nested batches, to its input field.
func fieldCommand(id uint64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			scoped := make([]tea.Cmd, 0, len(batch))
			for _, child := range batch {
				scoped = append(scoped, fieldCommand(id, child))
			}
			combined := tea.Batch(scoped...)
			if combined == nil {
				return nil
			}
			return combined()
		}
		return fieldMsg{id: id, message: msg}
	}
}

type terminalInput struct {
	*os.File
	closed func()
}

func (r terminalInput) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	if errors.Is(err, io.EOF) {
		r.closed()
	}
	return n, err
}

// The worker never writes to the terminal: only the model owns output and input.
type terminalBridge struct{ send func(tea.Msg) }

func (b *terminalBridge) Write(p []byte) (int, error) { b.send(lineMsg(string(p))); return len(p), nil }
func (b *terminalBridge) Screen(s string)             { b.send(screenMsg(s)) }
func (b *terminalBridge) Remember(s string)           { b.send(summaryMsg(s)) }
func (b *terminalBridge) Progress(e progress.Event)   { b.send(progressMsg{Event: e}) }
func (b *terminalBridge) Busy(s string) {
	b.send(progressMsg{Event: progress.Event{Stage: s}, keepText: true})
}
func (b *terminalBridge) Ask(ctx context.Context, p Prompt) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	request := promptMsg{prompt: p, reply: make(chan promptReply, 1)}
	b.send(request)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case reply := <-request.reply:
		return reply.value, reply.err
	}
}

type terminalModel struct {
	width, height int
	title         string
	lines         []string
	current       *progress.Event
	spinning      spinner.Model
	meter         bar.Model
	fieldID       uint64
	input         *textinput.Model
	request       *promptMsg
	validationErr error
	summary       string
	finished      bool
	err           error
	cancel        context.CancelFunc
	// A plain terminal keeps cooked echo for normal inputs. Only secret fields
	// temporarily use the platform's raw mode, with no renderer or ANSI output.
	plain       io.Writer
	in          *os.File
	secretState *term.State
}

func newTerminalModel(cancel context.CancelFunc, plain io.Writer) *terminalModel {
	return &terminalModel{width: 80, height: 24, cancel: cancel, plain: plain,
		spinning: spinner.New(spinner.WithSpinner(spinner.Line)), meter: bar.New(bar.WithColors(lipgloss.Color("81")))}
}
func (m *terminalModel) Init() tea.Cmd {
	if m.plain != nil {
		return nil
	}
	return m.spinning.Tick
}
func (m *terminalModel) restoreSecret() error {
	if m.secretState == nil {
		return nil
	}
	err := term.Restore(m.in.Fd(), m.secretState)
	m.secretState = nil
	return err
}
func (m *terminalModel) finishPrompt(err error) {
	if m.request == nil {
		return
	}
	err = errors.Join(err, m.restoreSecret())
	if m.plain != nil && m.request.prompt.Secret {
		fmt.Fprintln(m.plain)
	}
	value := ""
	if m.input != nil {
		value = m.input.Value()
	}
	m.request.reply <- promptReply{value: value, err: err}
	m.input = nil
	m.request = nil
	m.validationErr = nil
	m.lines = nil
	m.current = nil
}
func (m *terminalModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if scoped, ok := msg.(fieldMsg); ok {
		if m.input == nil || scoped.id != m.fieldID {
			return m, nil
		}
		msg = scoped.message
	}

	if key, ok := msg.(tea.KeyPressMsg); ok && m.plain != nil && (key.String() == "ctrl+j" || key.String() == "ctrl+m") {
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	}

	switch v := msg.(type) {
	case screenMsg:
		m.title = string(v)
		m.lines = nil
		m.current = nil
		if m.plain != nil {
			fmt.Fprintln(m.plain, "\n"+appinfo.Title+"\n"+m.title)
		}
	case lineMsg:
		if m.current != nil {
			m.current = nil
			m.lines = nil
		}
		m.lines = append(m.lines, strings.Split(strings.TrimSuffix(string(v), "\n"), "\n")...)
		if m.plain != nil {
			fmt.Fprint(m.plain, string(v))
		}
	case summaryMsg:
		m.summary = string(v)
	case progressMsg:
		if !v.keepText {
			m.lines = nil
		}
		if m.plain != nil && (m.current == nil || m.current.Stage != v.Stage || m.current.Item != v.Item) {
			fmt.Fprintln(m.plain, "  "+progressLabel(v.Event)+"...")
		}
		e := v.Event
		m.current = &e
	case promptMsg:
		m.current = nil
		m.validationErr = nil
		m.request = &v
		if m.plain != nil {
			// Configure masking before displaying the prompt, so early input cannot echo.
			if v.prompt.Secret && m.in != nil {
				state, err := term.MakeRaw(m.in.Fd())
				if err != nil {
					m.finishPrompt(err)
					return m, nil
				}
				m.secretState = state
			}
			fmt.Fprintln(m.plain, "\n"+v.prompt.Title)
			fmt.Fprint(m.plain, "> ")
		}
		return m, m.startInput()
	case tea.WindowSizeMsg:
		m.width = max(1, v.Width)
		m.height = max(4, v.Height)
		if m.input != nil {
			m.input.SetWidth(m.fieldWidth())
		}
	case inputClosedMsg:
		if m.request != nil {
			m.finishPrompt(ErrInputClosed)
			return m, nil
		}
		return m.Update(cancelMsg{})
	case cancelMsg:
		m.cancel()
		m.finishPrompt(context.Canceled)
		m.current = &progress.Event{Stage: "Останавливаем операцию"}
		return m, nil
	case workerDoneMsg:
		m.finished = true
		m.err = v.err
		m.current = nil
		m.finishPrompt(v.err)
		return m, tea.Quit
	case tea.KeyPressMsg:
		if v.String() == "ctrl+c" {
			return m.Update(cancelMsg{})
		}
		if m.input == nil {
			return m, nil
		}
		if v.String() == "ctrl+d" {
			m.finishPrompt(ErrInputClosed)
			return m, nil
		}
		if v.String() == "enter" {
			if validate := m.request.prompt.Validate; validate != nil {
				if err := validate(m.input.Value()); err != nil {
					m.validationErr = err
					if m.plain != nil {
						fmt.Fprintln(m.plain, appinfo.CleanDisplay(err.Error()))
						fmt.Fprint(m.plain, "> ")
						return m, m.startInput()
					}
					return m, nil
				}
			}
			m.finishPrompt(nil)
			return m, nil
		}
	}
	var cmds []tea.Cmd
	if m.input != nil {
		updated, cmd := m.input.Update(msg)
		*m.input = updated
		cmds = append(cmds, fieldCommand(m.fieldID, cmd))
	}
	if m.plain == nil {
		var cmd tea.Cmd
		m.spinning, cmd = m.spinning.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}
func (m *terminalModel) startInput() tea.Cmd {
	m.fieldID++
	m.input = inputField(m.request.prompt, m.fieldWidth())
	m.validationErr = nil
	return fieldCommand(m.fieldID, m.input.Focus())
}
func (m *terminalModel) fieldWidth() int { return max(8, min(76, m.width-2)) }
func (m *terminalModel) View() tea.View {
	if m.plain != nil {
		return tea.NewView("")
	}
	header := appinfo.Title
	if colorAllowed() {
		header = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Render(header)
	}
	var body string
	if m.finished {
		if m.err != nil && !errors.Is(m.err, ErrInputClosed) {
			body = appinfo.ErrorPrefix + " " + appinfo.CleanDisplay(m.err.Error())
		}
		if m.summary != "" {
			body += "\n" + m.summary
		}
		if strings.TrimSpace(body) == "" {
			body = "Работа завершена."
		}
	} else {
		body = strings.Join(m.lines, "\n")
		if m.title != "" {
			body = m.title + "\n\n" + body
		}
		if m.current != nil {
			e := *m.current
			body += "\n"
			if e.Total > 0 {
				body += progressLabel(e) + "\n\n"
				m.meter.SetWidth(max(4, min(48, m.width-4)))
				body += m.meter.ViewAs(min(1, max(0, float64(e.Done)/float64(e.Total))))
			} else {
				body += m.spinning.View() + " " + progressLabel(e)
			}
		}
		if m.input != nil {
			body += "\n" + m.request.prompt.Title + "\n" + m.input.View()
			if m.validationErr != nil {
				body += "\n" + appinfo.CleanDisplay(m.validationErr.Error())
			}
		}
	}
	// Wrap with display-width-aware library code; keep the current prompt visible.
	body = ansi.Hardwrap(strings.TrimSpace(body), max(1, m.width-2), true)
	lines := strings.Split(body, "\n")
	available := max(1, m.height-4)
	if len(lines) > available {
		lines = lines[len(lines)-available:]
	}
	content := ansi.Truncate(header, m.width, "") + "\n\n" + strings.Join(lines, "\n") + "\n"
	if !colorAllowed() {
		content = ansi.Strip(content)
	}
	return tea.NewView(content)
}

func runTerminal(ctx context.Context, o Options, action string) error {
	return terminalSession(ctx, o, func(ctx context.Context, options Options) error {
		if action == "" {
			return runMenu(ctx, options)
		}
		return runAction(ctx, action, options)
	})
}
func terminalSession(ctx context.Context, o Options, work func(context.Context, Options) error) error {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Input == nil || !term.IsTerminal(o.Input.Fd()) {
		return errors.New("откройте утилиту в терминале")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var plain io.Writer
	outFile, isFile := o.Out.(*os.File)
	if os.Getenv("TERM") == "dumb" || !isFile || !term.IsTerminal(outFile.Fd()) {
		plain = o.Out
	}
	bridge := new(terminalBridge)
	completed := make(chan error, 1)
	model := newTerminalModel(cancel, plain)
	model.in = o.Input
	var program *tea.Program
	var input io.Reader = o.Input
	if plain != nil {
		input = terminalInput{File: o.Input, closed: func() { program.Send(inputClosedMsg{}) }}
	}
	opts := []tea.ProgramOption{tea.WithInput(input), tea.WithOutput(o.Out), tea.WithoutSignalHandler()}
	if plain != nil {
		opts = append(opts, tea.WithoutRenderer())
	}
	program = tea.NewProgram(model, opts...)
	bridge.send = program.Send
	stop := context.AfterFunc(ctx, func() { program.Send(cancelMsg{}) })
	defer stop()
	go func() {
		inner := o
		inner.Out = bridge
		inner.Prompts = bridge
		err := work(workerCtx, inner)
		completed <- err
		program.Send(workerDoneMsg{err: err})
	}()
	_, runErr := program.Run()
	cancel()
	// Join the worker on every exit, including terminal startup/input failures.
	workerErr := <-completed
	restoreErr := model.restoreSecret()
	if plain != nil && model.summary != "" {
		fmt.Fprintln(plain, model.summary)
	}
	return endInput(errors.Join(runErr, workerErr, restoreErr))
}
