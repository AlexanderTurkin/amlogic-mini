// Package cli exposes the three shared menu operations as command shortcuts.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"amlogicmini/internal/appinfo"
)

// Version is set by the release linker.
var Version = "0.4.0-dev"

type ActionRunner func(context.Context, string) error

func help(out io.Writer) {
	fmt.Fprintf(out, "%s\n\nБез аргументов — меню с цифрами.\n\nКоманды:\n", appinfo.Title)
	for _, action := range appinfo.Actions() {
		fmt.Fprintf(out, "  amlogic-mini %-10s%s\n", action.Name, action.Label)
	}
	fmt.Fprint(out, `
Каждая команда открывает те же шаги и подтверждения, что и пункт меню.
Папки backups и alpine находятся рядом с программой. Ввод путей не нужен.
Для запуска операций нужен интерактивный терминал.

  amlogic-mini --help    Справка
  amlogic-mini version   Версия
  amlogic-mini --licenses Лицензии зависимостей
`)
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, run ActionRunner) int {
	if len(args) == 0 {
		help(stdout)
		return 0
	}
	name := args[0]
	_, isAction := appinfo.Find(name)
	if len(args) == 2 && isAction && (args[1] == "--help" || args[1] == "-h") {
		help(stdout)
		return 0
	}
	if len(args) != 1 {
		return Status(stderr, fmt.Errorf("лишние параметры; используйте amlogic-mini --help"))
	}
	switch name {
	case "help", "--help", "-h":
		help(stdout)
		return 0
	case "--licenses":
		fmt.Fprint(stdout, appinfo.Notices)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "%s %s\n", appinfo.Title, Version)
		return 0
	}
	if !isAction {
		return Status(stderr, fmt.Errorf("неизвестная команда %q; используйте amlogic-mini --help", name))
	}
	if run == nil {
		return Status(stderr, fmt.Errorf("не настроен запуск операций"))
	}
	if err := run(ctx, name); err != nil {
		return Status(stderr, err)
	}
	return 0
}

// Status reports a command failure and returns its process exit code.
func Status(out io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(out, appinfo.ErrorPrefix, appinfo.CleanDisplay(err.Error()))
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}
