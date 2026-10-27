//go:build windows && amlogic_mini_driver

package usbdriver

/*
#cgo pkg-config: libwdi
#include <windows.h>
#include <shellapi.h>
#include <shlobj.h>
#include <newdev.h>
#include <setupapi.h>
#include <stdint.h>
#include <stdlib.h>
#include <libwdi.h>

static DWORD amlogic_mini_elevate(const wchar_t* exe, const wchar_t* args, HANDLE* process) {
 SHELLEXECUTEINFOW info = {0};
 info.cbSize = sizeof(info);
 info.fMask = SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NOASYNC | SEE_MASK_FLAG_NO_UI;
 info.lpVerb = L"runas";
 info.lpFile = exe;
 info.lpParameters = args;
 info.nShow = SW_HIDE;
 if (!ShellExecuteExW(&info)) return GetLastError();
 *process = info.hProcess;
 return 0;
}

#define AMLOGIC_MINI_WDI_ERROR_BASE 0x20000000
static DWORD amlogic_mini_prepare_driver(const char* path, wchar_t* published, DWORD capacity) {
 if (!IsUserAnAdmin()) return ERROR_ELEVATION_REQUIRED;
 struct wdi_device_info device = {0};
 struct wdi_options_prepare_driver options = {0};
 device.vid = 0x1b8e;
 device.pid = 0xc003;
 device.desc = "Amlogic A113X Bootloader";
 options.driver_type = WDI_WINUSB;
 wdi_set_log_level(WDI_LOG_LEVEL_ERROR);
 int result = wdi_prepare_driver(&device, path, "amlogic_a113x.inf", &options);
 if (result != WDI_SUCCESS) return AMLOGIC_MINI_WDI_ERROR_BASE + (DWORD)(-result);
 wchar_t inf[MAX_PATH];
 if (!MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, inf, MAX_PATH)) return GetLastError();
 if (wcscat_s(inf, MAX_PATH, L"\\amlogic_a113x.inf") != 0) return ERROR_FILENAME_EXCED_RANGE;
 BOOL reboot = FALSE;
 // Windows stages the signed package synchronously; no nested installer/pipe.
 HMODULE module = LoadLibraryExW(L"newdev.dll", NULL, LOAD_LIBRARY_SEARCH_SYSTEM32);
 if (!module) return GetLastError();
 typedef BOOL (WINAPI *install_fn)(HWND, LPCWSTR, DWORD, PBOOL);
 install_fn install = (install_fn)GetProcAddress(module, "DiInstallDriverW");
 if (!install) { FreeLibrary(module); return ERROR_PROC_NOT_FOUND; }
 BOOL installed = install(NULL, inf, DIIRFLAG_FORCE_INF, &reboot);
 DWORD error = installed ? ERROR_SUCCESS : GetLastError();
 FreeLibrary(module);
 if (!installed) return error;
 if (reboot) return ERROR_SUCCESS_REBOOT_REQUIRED;
 if (!SetupCopyOEMInfW(inf, NULL, SPOST_PATH, 0, published, capacity, NULL, NULL)) return GetLastError();
 return ERROR_SUCCESS;
}

*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const helperArgument = "--amlogic-mini-install-usb-driver"

func New() *Installer { return &Installer{} }

// HandleCommand is a private fixed-action child entry, never a shell or file runner.
func HandleCommand(args []string) (bool, int) {
	if len(args) != 1 || args[0] != helperArgument {
		return false, 0
	}
	name, _ := windows.UTF16PtrFromString(`Global\AmlogicA113XMini.WinUSB`)
	guard, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if guard != 0 {
			windows.CloseHandle(guard)
		}
		if err == windows.ERROR_ALREADY_EXISTS {
			return true, int(windows.ERROR_INSTALL_ALREADY_RUNNING)
		}
		return true, int(windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(guard)
	dir, err := os.MkdirTemp("", "amlogic-mini-driver-")
	if err != nil {
		return true, 1
	}
	defer os.RemoveAll(dir)
	path := C.CString(dir)
	defer C.free(unsafe.Pointer(path))
	var published [windows.MAX_PATH]uint16
	result := C.amlogic_mini_prepare_driver(path, (*C.wchar_t)(unsafe.Pointer(&published[0])), C.DWORD(len(published)))
	if result != 0 {
		return true, int(result)
	}
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, settingsKey, registry.SET_VALUE)
	if err != nil {
		return true, int(windows.ERROR_WRITE_FAULT)
	}
	defer key.Close()
	if err := key.SetStringValue("WinUSBInf", windows.UTF16ToString(published[:])); err != nil {
		return true, int(windows.ERROR_WRITE_FAULT)
	}
	return true, 0
}

func (*Installer) Install(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args, _ := windows.UTF16PtrFromString(helperArgument)
	var process C.HANDLE
	status := C.amlogic_mini_elevate((*C.wchar_t)(unsafe.Pointer(file)), (*C.wchar_t)(unsafe.Pointer(args)), &process)
	if status != 0 {
		return fmt.Errorf("запрос прав администратора: %w", windows.Errno(status))
	}
	handle := windows.Handle(uintptr(unsafe.Pointer(process)))
	defer windows.CloseHandle(handle)
	// Never terminate an installer midway. Cancellation waits for its cleanup,
	// then the wizard observes the original context cause before opening USB.
	if _, err := windows.WaitForSingleObject(handle, windows.INFINITE); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return err
	}
	if code == uint32(windows.ERROR_SUCCESS_REBOOT_REQUIRED) {
		return fmt.Errorf("драйвер установлен; перезагрузите Windows и повторите операцию")
	}
	if code >= C.AMLOGIC_MINI_WDI_ERROR_BASE && code < C.AMLOGIC_MINI_WDI_ERROR_BASE+256 {
		reason := C.GoString(C.wdi_strerror(-C.int(code - C.AMLOGIC_MINI_WDI_ERROR_BASE)))
		return fmt.Errorf("подготовка пакета WinUSB: %s (код %d)", reason, code-C.AMLOGIC_MINI_WDI_ERROR_BASE)
	}
	if code != 0 {
		return fmt.Errorf("установка WinUSB: %w (код 0x%08x)", windows.Errno(code), code)
	}
	return nil
}
