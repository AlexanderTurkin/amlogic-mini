//go:build windows && amlogic_mini_driver

package usbdriver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const settingsKey = `SOFTWARE\AmlogicA113XMini`

func (*Installer) Ready(ctx context.Context) (bool, error) {
	return checkReady(ctx, lookupBinding, stagedPackage)
}

// Present devices take precedence; phantom records are only a fallback for
// installations made by another tool before this utility saved a package path.
func lookupBinding(ctx context.Context, present bool) (binding, error) {
	flags := windows.DIGCF_ALLCLASSES
	if present {
		flags |= windows.DIGCF_PRESENT
	}
	devices, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, flags, 0, "")
	if err != nil {
		return binding{}, err
	}
	defer devices.Close()
	result := binding{}
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return binding{}, err
		}
		device, err := devices.EnumDeviceInfo(index)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			return result, nil
		}
		if err != nil {
			return binding{}, err
		}
		value, err := devices.DeviceRegistryProperty(device, windows.SPDRP_HARDWAREID)
		if err != nil {
			continue
		}
		ids, ok := value.([]string)
		if !ok {
			continue
		}
		target := false
		for _, id := range ids {
			if isTarget(id) {
				target = true
				break
			}
		}
		if !target {
			continue
		}
		service, err := devices.DeviceRegistryProperty(device, windows.SPDRP_SERVICE)
		if err != nil && !errors.Is(err, windows.ERROR_INVALID_DATA) {
			return binding{}, fmt.Errorf("read USB driver: %w", err)
		}
		name, _ := service.(string)
		configured := strings.EqualFold(name, "WinUSB")
		if present && !configured {
			return binding{found: true}, nil
		}
		result.found = true
		result.ready = result.ready || configured
	}
}

// This admin-owned path is recorded only after successful Windows staging.
// Ask SetupAPI whether the published INF still belongs to the driver store.
func stagedPackage() (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, settingsKey, registry.QUERY_VALUE)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	path, _, err := key.GetStringValue("WinUSBInf")
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	input, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	buffer := make([]uint16, windows.MAX_PATH)
	var required uint32
	api := windows.NewLazySystemDLL("setupapi.dll").NewProc("SetupGetInfDriverStoreLocationW")
	ok, _, callErr := api.Call(uintptr(unsafe.Pointer(input)), 0, 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&required)))
	if ok == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(callErr, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, callErr
	}
	return strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer)), "amlogic_a113x.inf"), nil
}
