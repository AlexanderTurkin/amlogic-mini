package wizard

import (
	"context"
	"errors"
	"fmt"
)

// USBDriver is optional: only Windows releases need first-use preparation.
type USBDriver interface {
	Ready(context.Context) (bool, error)
	Install(context.Context) error
}

func (w *wizard) prepareUSBDriver(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.USBDriver == nil {
		return nil
	}
	w.busy("Проверяем драйвер USB")
	ready, err := w.USBDriver.Ready(ctx)
	if err != nil {
		return fmt.Errorf("проверка драйвера USB: %w", err)
	}
	if ready {
		return nil
	}
	w.line("\n  Для загрузчика устройства (1b8e:c003) нужен WinUSB.")
	w.line("  Подготовим драйвер до подключения устройства.")
	w.line("  Windows может запросить права администратора.")
	w.line("  Будет добавлен локальный сертификат пакета драйвера.")
	yes, err := w.confirm(ctx, "НАСТРОИТЬ USB ДЛЯ УСТРОЙСТВА?")
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("настройка USB отменена")
	}
	w.busy("Подготавливаем драйвер WinUSB")
	if err := w.USBDriver.Install(ctx); err != nil {
		return fmt.Errorf("настройка WinUSB: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.line("\n  Драйвер подготовлен. Теперь переподключите устройство.")
	return nil
}
