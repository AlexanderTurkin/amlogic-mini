package wizard

import (
	"context"
	"errors"
	"time"

	"amlogicmini/internal/appinfo"

	"amlogicmini/internal/onboarding"
)

const (
	sshCommand          = "ssh root@192.168.14.14"
	initialPasswordHint = "Начальный пароль: 1. Смените его командой passwd."
)

func (w *wizard) setup(ctx context.Context) error {
	w.rememberSSH(false)
	w.line("\n  Отключите USB от устройства на 5 секунд и подключите снова.")
	n, err := w.choice(ctx, "ПРОВЕРКА ЗАПУСКА ALPINE", []string{"USB переподключён — проверить SSH"}, "Пропустить проверку")
	if err != nil {
		return err
	}
	if n == 0 {
		w.line("  Запуск Alpine и SSH пока не проверены.")
		w.connectionHint(true)
		return nil
	}
	client := w.NewSSH()
	password := "1"
	for {
		w.busy("Ожидаем загрузку и проверяем SSH (до 2 минут)")
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		info, checkErr := client.Wait(waitCtx, password)
		cancel()
		if checkErr == nil {
			w.line("\n  [OK] Alpine " + appinfo.CleanDisplay(info.Version) + " загрузился")
			w.line("  [OK] Вход по SSH проверен")
			w.line("  Ключ SSH (первое подключение через USB):")
			w.line("  " + appinfo.CleanDisplay(info.HostKey))
			return w.passwordSetup(ctx, client, password)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.line("\n  Прошивка проверена, но вход по SSH пока недоступен.")
		w.line("  " + appinfo.CleanDisplay(checkErr.Error()))
		if errors.Is(checkErr, onboarding.ErrHostKeyChanged) {
			w.line("  Не продолжайте, пока не проверите подключённое устройство.")
			w.connectionHint(false)
			return nil
		}
		n, err = w.choice(ctx, "ЧТО ДЕЛАТЬ ДАЛЬШЕ?", []string{"Повторить проверку", "Ввести текущий пароль"}, "Пропустить")
		if err != nil {
			return err
		}
		if n == 0 {
			w.connectionHint(password == "1")
			return nil
		}
		if n == 2 {
			password, err = w.Prompts.Ask(ctx, Prompt{Title: "Текущий пароль root (ввод скрыт)", Secret: true})
			if err != nil {
				return err
			}
		}
	}
}
func (w *wizard) passwordSetup(ctx context.Context, client SSH, current string) error {
	for {
		w.line("\n  Рекомендуем установить свой пароль root.")
		n, err := w.choice(ctx, "СМЕНА ПАРОЛЯ", []string{"Сменить пароль"}, "Пропустить")
		if err != nil {
			return err
		}
		if n == 0 {
			w.connectionHint(current == "1")
			return nil
		}
		next, err := w.Prompts.Ask(ctx, Prompt{Title: "Новый пароль (8–128 символов, ввод скрыт)", Secret: true, Validate: onboarding.ValidatePassword})
		if err != nil {
			return err
		}
		_, err = w.Prompts.Ask(ctx, Prompt{Title: "Повторите новый пароль", Secret: true, Validate: func(s string) error {
			if s != next {
				return errors.New("пароли не совпадают")
			}
			return nil
		}})
		if err != nil {
			return err
		}
		w.busy("Меняем пароль и проверяем новый вход")
		changeErr := client.ChangePassword(ctx, current, next)
		next = "" // Do not retain a second reference longer than the request needs.
		if ctx.Err() != nil {
			w.line("  Смена прервана: пароль мог уже измениться.")
			return ctx.Err()
		}
		if changeErr == nil {
			w.line("\n  Пароль изменён. Вход с новым паролем проверен.")
			w.connectionHint(false)
			return nil
		}
		w.line("\n  " + appinfo.CleanDisplay(changeErr.Error()))
		w.line("  Автоматический откат пароля не выполнялся.")
		n, err = w.choice(ctx, "ПРОВЕРЬТЕ ТЕКУЩИЙ ПАРОЛЬ", []string{"Ввести текущий пароль и проверить вход"}, "Завершить настройку")
		if err != nil {
			return err
		}
		if n == 0 {
			w.connectionHint(false)
			return nil
		}
		current, err = w.Prompts.Ask(ctx, Prompt{Title: "Текущий пароль root (ввод скрыт)", Secret: true})
		if err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, checkErr := client.Wait(checkCtx, current)
		cancel()
		if checkErr != nil {
			w.line("  Вход не подтверждён: " + appinfo.CleanDisplay(checkErr.Error()))
			w.connectionHint(false)
			return nil
		}
	}
}
func (w *wizard) rememberSSH(defaultPassword bool) {
	hint := "Alpine записан.\nПодключение через USB: " + sshCommand
	if defaultPassword {
		hint += "\n" + initialPasswordHint
	}
	w.remember(hint)
}
func (w *wizard) connectionHint(defaultPassword bool) {
	w.rememberSSH(defaultPassword)
	w.line("\n  ПОДКЛЮЧЕНИЕ К УСТРОЙСТВУ\n")
	w.line("      " + sshCommand + "\n")
	w.line("  Адрес доступен при USB-подключении.")
	if defaultPassword {
		w.line("  " + initialPasswordHint)
	} else {
		w.line("  Используйте свой текущий пароль root.")
	}
	w.line("  Перед отключением питания в Alpine выполните sync.")
	w.line("")
}
