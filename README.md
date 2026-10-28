# Amlogic A113X Mini

CLI-инструмент для резервного копирования, восстановления и прошивки Alpine Linux
на совместимых устройствах с Amlogic A113X и NAND 256 МиБ.
Совместимость определяется загрузчиком, контроллером NAND и разметкой памяти.

Запуск: `amlogic-mini` (Windows: `amlogic-mini.exe`). Без аргументов — меню.
Команды: `backup`, `restore`, `flash`.

- `backups/` — резервные копии формата 2. Старые форматы не поддерживаются.
- `alpine/` — `Image`, `a113x.dtb` и ровно один образ rootfs `.ubi`.

Перед прошивкой нужна резервная копия. Программа подскажет момент подключения
USB и предложит настроить WinUSB на Windows.

Сборки: macOS Apple Silicon, Windows 11 x64 и Linux x86-64.
На Windows используется штатный WinUSB; файлы драйверов Microsoft в архивы не входят.
Linux-бинарь статический, со встроенной libusb: отдельные версии для Ubuntu,
Debian/Mint, Fedora, Arch и Alpine не нужны. Для доступа к USB запускайте
`sudo ./amlogic-mini` либо заранее настройте права через udev.
На macOS запускайте `sudo ./amlogic-mini`: без root (или подписи с entitlement
`com.apple.vm.device-access`) libusb не может захватить USB-интерфейс.

Собственный код — [MIT](LICENSE). Все сторонние лицензии, сведения об авторах
и патентные разрешения доступны офлайн: `amlogic-mini --licenses`.
Исходники libusb (LGPL-2.1-or-later) и libwdi (LGPL-3.0-or-later) — в `third_party/`;
изменения libwdi — в `scripts/libwdi-inbox-winusb.patch`.
При распространении сборок сохраняйте доступ к исходникам этого же релиза,
включая `third_party/` и скрипты пересборки.

Пересборка на macOS: Go 1.26.x, Zig 0.16.0, clang, pkg-config, Python 3.13 и patch:

```sh
python3 scripts/release.py --target darwin-arm64 --target windows-amd64 --target linux-amd64 --work /tmp/amlogic-mini-build
```

Для изменения и перелинковки библиотек редактируйте распакованные исходники
в `--work` и пересоберите. Перед пересборкой libusb удалите в этой папке
`<target>/prefix/lib/libusb-1.0.a` (на Windows — `<target>/native-prefix/lib/libusb-1.0.a`).
Скрипт повторно собирает libwdi и приложение; исходные архивы остаются неизменными.
