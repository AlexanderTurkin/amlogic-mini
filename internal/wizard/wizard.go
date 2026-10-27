// Package wizard provides the numbered, ordinary-terminal interface.
package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"amlogicmini/internal/appinfo"
	"amlogicmini/internal/backup"
	"amlogicmini/internal/onboarding"
)

type Opener func(context.Context, time.Duration) (backup.Device, [7]byte, func() error, error)
type SSH interface {
	Wait(context.Context, string) (onboarding.Info, error)
	ChangePassword(context.Context, string, string) error
}
type Options struct {
	Home, Version string
	Input         *os.File
	Out           io.Writer
	Prompts       Prompts
	Open          Opener
	NewSSH        func() SSH
	USBDriver     USBDriver
}
type wizard struct {
	Options
	completed bool
}

func newWizard(o Options) *wizard {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.NewSSH == nil {
		o.NewSSH = func() SSH { return onboarding.New(onboarding.USBAddress) }
	}
	return &wizard{Options: o}
}
func prepare(o Options) (*wizard, error) {
	w := newWizard(o)
	if w.Prompts == nil || w.Open == nil {
		return nil, errors.New("не настроен интерфейс или доступ к USB")
	}
	if w.Home == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("не удалось найти папку программы: %w", err)
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return nil, fmt.Errorf("не удалось найти файл программы: %w", err)
		}
		w.Home = filepath.Dir(executable)
	}

	return w, nil
}

// RunAction enters the exact operation used by a menu item, then returns.
func RunAction(ctx context.Context, action string, o Options) error {
	if o.Input != nil {
		return runTerminal(ctx, o, action)
	}
	return runAction(ctx, action, o)
}
func runAction(ctx context.Context, action string, o Options) error {
	w, err := prepare(o)
	if err != nil {
		return err
	}
	return endInput(w.perform(ctx, action, "Нажмите Enter, чтобы завершить"))
}

// perform is the shared operation/result flow; callers decide where to go next.
func (w *wizard) perform(ctx context.Context, action, after string) error {
	err := w.runAction(ctx, action)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrInputClosed) {
		return err
	}
	if err != nil {
		w.line("\n" + appinfo.ErrorPrefix + " " + appinfo.CleanDisplay(err.Error()))
	}
	if err != nil || w.completed {
		_, pauseErr := w.Prompts.Ask(ctx, Prompt{Title: after})
		if errors.Is(pauseErr, ErrInputClosed) {
			if err != nil {
				return err
			}
			return pauseErr
		}
		err = errors.Join(err, pauseErr)
	}
	return err
}

func (w *wizard) runAction(ctx context.Context, action string) error {
	w.completed = false
	definition, known := appinfo.Find(action)
	if !known {
		return fmt.Errorf("неизвестное действие: %s", appinfo.CleanDisplay(action))
	}
	w.screen(definition.Title)
	switch action {
	case appinfo.Backup:
		return w.backup(ctx)
	case appinfo.Restore:
		return w.restore(ctx)
	case appinfo.Flash:
		return w.flash(ctx)
	default:
		return fmt.Errorf("нет обработчика действия %q", action)
	}
}
func Run(ctx context.Context, o Options) error {
	if o.Input != nil {
		return runTerminal(ctx, o, "")
	}
	return runMenu(ctx, o)
}
func runMenu(ctx context.Context, o Options) error {
	w, err := prepare(o)
	if err != nil {
		return err
	}

	w.welcome()
	if _, err := w.Prompts.Ask(ctx, Prompt{Title: "Нажмите Enter, чтобы открыть меню"}); err != nil {
		return endInput(err)
	}
	actions := appinfo.Actions()
	items := make([]string, len(actions))
	for i, action := range actions {
		items[i] = action.Label
	}
	for {
		w.screen("")
		choice, err := w.choice(ctx, "ГЛАВНОЕ МЕНЮ", items, "Выйти")
		if err != nil {
			return endInput(err)
		}
		if choice == 0 {
			return nil
		}
		action := actions[choice-1].Name
		err = w.perform(ctx, action, "Нажмите Enter, чтобы вернуться в меню")

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrInputClosed) {
				return nil
			}
		}

	}
}
func endInput(err error) error {
	if errors.Is(err, ErrInputClosed) {
		return nil
	}
	return err
}
func (w *wizard) screen(title string) {
	if ui, ok := w.Out.(presentation); ok {
		ui.Screen(title)
	}
}
func (w *wizard) busy(label string) {
	if ui, ok := w.Out.(presentation); ok {
		ui.Busy(label)
	}
}
func (w *wizard) remember(summary string) {
	if ui, ok := w.Out.(presentation); ok {
		ui.Remember(summary)
	}
}
func (w *wizard) line(s string) { fmt.Fprintln(w.Out, s) }
func (w *wizard) welcome() {
	w.screen("")
	compact := false
	if w.Input != nil {
		if width, height, err := term.GetSize(w.Input.Fd()); err == nil {
			compact = width < 62 || height < 24
		}
	}
	if !compact {
		w.line(`
             | | | | | | | |
         +----------------------+
       --|    AMLOGIC A113X     |--
       --|     ALPINE LINUX     |--
         +----------------------+
             | | | | | | | |`)
	}
	if _, ok := w.Out.(presentation); !ok {
		w.line("\n               " + appinfo.Title + "\n")
	}
	w.line("  " + appinfo.Description + "\n")
}
func (w *wizard) choice(ctx context.Context, title string, items []string, zero string) (int, error) {
	w.line("\n  " + title + "\n")
	for i, item := range items {
		w.line(fmt.Sprintf("  %d. %s", i+1, appinfo.CleanDisplay(item)))
	}
	w.line("\n  0. " + zero + "\n")
	validate := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 || n > len(items) {
			return fmt.Errorf("введите цифру от 0 до %d", len(items))
		}
		return nil
	}
	s, err := w.Prompts.Ask(ctx, Prompt{Title: "Ваш выбор (Enter — " + strings.ToLower(zero) + ")", Validate: validate})
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err
}
func (w *wizard) confirm(ctx context.Context, title string) (bool, error) {
	n, err := w.choice(ctx, title, []string{"Продолжить"}, "Отмена")
	return n == 1, err
}

// Engines retain their storage-identity checks while a menu operation runs.
// The temporary diagnostic file is not part of the user's portable folder.
func temporaryReport(kind string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "amlogic-mini-"+kind+"-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) } // Best-effort cleanup of our private temporary directory.
	return filepath.Join(dir, "operation.json"), cleanup, nil
}
func (w *wizard) connect(ctx context.Context) (backup.Device, [7]byte, func() error, error) {
	if err := w.prepareUSBDriver(ctx); err != nil {
		return nil, [7]byte{}, nil, err
	}
	w.line("\n  Переподключите устройство USB-кабелем с передачей данных.")
	w.busy("Ждём подключения устройства (до 2 минут)")
	d, version, closeDevice, err := w.Open(ctx, 2*time.Minute)
	if err == nil && closeDevice == nil {
		err = errors.New("USB не предоставил функцию отключения")
	}
	return d, version, closeDevice, err
}
