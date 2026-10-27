package wizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"amlogicmini/internal/appinfo"

	"amlogicmini/internal/backup"
	"amlogicmini/internal/flash"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/progress"
	"amlogicmini/internal/restore"
)

func (w *wizard) backup(ctx context.Context) (err error) {
	dir := filepath.Join(w.Home, "backups", time.Now().Format("2006-01-02_15-04-05.000000000"))
	w.line("\n  Копия будет сохранена рядом с программой:")
	w.line("  " + appinfo.CleanDisplay(dir))
	w.line("\n  Сохраним данные и проверим копию. Устройство не изменится.")
	w.line("  Копия содержит личные данные и ключи. Не передавайте её другим.")
	yes, err := w.confirm(ctx, "СОЗДАТЬ РЕЗЕРВНУЮ КОПИЮ?")
	if err != nil || !yes {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return portableWriteError(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return portableWriteError(err)
	}
	d, fw, closeDevice, err := w.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeDevice()) }()
	_, err = backup.Run(ctx, d, backup.Options{Output: dir, Profile: profile.A113X(), Firmware: fw, Version: w.Version, Progress: w.progress})
	if err != nil {
		return fmt.Errorf("копия не завершена; файлы сохранены в %s: %w", dir, err)
	}
	w.completed = true
	w.remember("Резервная копия сохранена и проверена:\n" + appinfo.CleanDisplay(dir))
	w.line("\n  Резервная копия сохранена и проверена.")
	w.line("  Папка: " + appinfo.CleanDisplay(dir))
	return nil
}
func (w *wizard) backupDirectories() ([]string, error) {
	root := filepath.Join(w.Home, "backups")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			p := filepath.Join(root, entry.Name())
			paths = append(paths, p)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	return paths, nil
}
func (w *wizard) selectBackup(ctx context.Context) (string, error) {
	directories, err := w.backupDirectories()
	if err != nil {
		return "", err
	}
	var paths []string
	for _, p := range directories {
		if info, e := os.Stat(filepath.Join(p, "manifest.json")); e == nil && info.Mode().IsRegular() {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		w.line("\n  В папке backups рядом с программой нет резервных копий.")
		w.line("  Сначала выберите пункт 1 в главном меню и создайте копию.")
		_, err := w.Prompts.Ask(ctx, Prompt{Title: "Нажмите Enter, чтобы вернуться"})
		return "", err
	}
	w.line("\n  Выбранную копию проверим перед продолжением.")
	const pageSize = 8
	for page := 0; ; {
		end := min(page+pageSize, len(paths))
		items := make([]string, 0, pageSize+2)
		for _, p := range paths[page:end] {
			items = append(items, filepath.Base(p))
		}
		next, previous := 0, 0
		if end < len(paths) {
			items = append(items, "Следующие копии")
			next = len(items)
		}
		if page > 0 {
			items = append(items, "Предыдущие копии")
			previous = len(items)
		}
		n, err := w.choice(ctx, "ВЫБОР РЕЗЕРВНОЙ КОПИИ", items, "Назад")
		if err != nil || n == 0 {
			return "", err
		}
		switch n {
		case next:
			page += pageSize
		case previous:
			page -= pageSize
		default:
			return paths[page+n-1], nil
		}
	}
}
func portableWriteError(err error) error {
	return fmt.Errorf("не удалось сохранить файлы рядом с программой; перенесите всю папку программы в доступное для записи место: %w", err)
}

func (w *wizard) restore(ctx context.Context) (err error) {
	path, err := w.selectBackup(ctx)
	if err != nil || path == "" {
		return err
	}
	w.busy("Проверяем файлы резервной копии")
	s, err := restore.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()

	w.line("\n  Текущая система и данные будут заменены выбранной копией.")
	yes, err := w.confirm(ctx, "ВОССТАНОВИТЬ УСТРОЙСТВО?")
	if err != nil || !yes {
		return err
	}
	report, cleanup, err := temporaryReport("restore")
	if err != nil {
		return err
	}
	defer cleanup()
	d, _, closeDevice, err := w.connect(ctx)
	if err != nil {
		return err
	}
	// Release USB before suggesting power-cycle.
	writer, ok := d.(restore.Device)
	if !ok {
		return errors.Join(errors.New("устройство не поддерживает восстановление"), closeDevice())
	}
	_, applyErr := s.Apply(ctx, writer, restore.Options{ConfirmDevice: s.Plan.Fingerprint, AllowPartialData: s.Plan.PartialData, Report: report, Progress: w.progress})
	err = errors.Join(applyErr, closeDevice())
	if err != nil {
		return writeFailure(err)
	}
	w.completed = true
	w.remember("Восстановление записано и проверено.")
	w.line("\n  Восстановление записано и проверено.")
	w.line("  Отключите USB и подключите блок питания устройства.")
	return nil
}
func (w *wizard) flash(ctx context.Context) (err error) {
	copies, err := w.backupDirectories()
	if err != nil {
		return err
	}
	if len(copies) == 0 {
		w.line("\n  В папке backups нет резервных копий. Сначала создайте копию.")
		return w.backup(ctx)
	}
	saved := copies[0]

	images := filepath.Join(w.Home, "alpine")
	if info, err := os.Stat(images); err != nil || !info.IsDir() {
		return fmt.Errorf("прошивка недоступна: рядом с программой нужна папка alpine с файлами Image, a113x.dtb и одним rootfs .ubi")
	}
	w.line("  Образы из папки: " + appinfo.CleanDisplay(images))
	w.busy("Проверяем образы Alpine")
	s, err := flash.OpenWithBackupPresence(ctx, images, saved)
	if err != nil {
		return err
	}
	w.line("\n  [OK] Резервная копия найдена")
	w.line("  [OK] Образы Alpine проверены")
	w.line("\n  Текущая система и данные будут удалены.")
	w.line("  Будет установлен Alpine Linux.")
	yes, err := w.confirm(ctx, "УСТАНОВИТЬ ALPINE LINUX?")
	if err != nil || !yes {
		return err
	}
	report, cleanup, err := temporaryReport("flash-alpine")
	if err != nil {
		return err
	}
	defer cleanup()
	d, _, closeDevice, err := w.connect(ctx)
	if err != nil {
		return err
	}
	writer, ok := d.(flash.Device)
	if !ok {
		return errors.Join(errors.New("устройство не поддерживает прошивку"), closeDevice())
	}
	_, applyErr := s.Apply(ctx, writer, flash.Options{BackupPresenceConfirmed: true, Report: report, Progress: w.progress})
	err = errors.Join(applyErr, closeDevice())
	if err != nil {
		return writeFailure(err)
	}
	w.completed = true
	w.remember("Alpine записан. Все три образа проверены.")
	w.line("\n  Alpine записан. Все три образа проверены.")
	return w.setup(ctx)
}
func writeFailure(err error) error {
	return fmt.Errorf("операция остановлена. Оставьте USB подключённым для восстановления. Не отключайте питание во время записи: %w", err)
}
func (w *wizard) progress(event progress.Event) {
	if ui, ok := w.Out.(presentation); ok {
		ui.Progress(event)
		return
	}
	w.line("  " + progressLabel(event))
}
func progressLabel(e progress.Event) string {
	labels := map[string]string{"files": "Проверяем сохранённые файлы", "source": "Проверяем данные перед записью", "check": "Проверяем загрузчик и устройство", "environment": "Читаем настройки загрузки", "read": "Читаем", "verify": "Читаем для проверки", "write": "Передаём", "raw": "Читаем физическую память", "erase": "Подготавливаем память", "boot": "Проверяем настройки загрузки"}
	label := labels[e.Stage]
	if label == "" {
		label = e.Stage
	}
	if e.Item != "" {
		label += ": " + e.Item
	}
	return appinfo.CleanDisplay(label)
}
