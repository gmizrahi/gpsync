//go:build windows

package main

import (
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/windows/registry"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// autostartAdapter satisfies internal/dashboard's AutostartController --
// a two-line wrapper around the three package-level functions below, so
// dashboard.Handler never touches the Windows registry (or anything
// platform-specific) directly, only through this seam.
type autostartAdapter struct{}

func (autostartAdapter) Installed() (bool, error)     { return autostartInstalled() }
func (autostartAdapter) Install(exePath string) error { return installAutostart(exePath) }
func (autostartAdapter) Uninstall() error             { return uninstallAutostart() }

// autostartRegistryKey is the per-user Run key -- HKEY_CURRENT_USER, not
// LOCAL_MACHINE, so this needs no elevation and matches the project's
// existing no-installer design (see Makefile/deploy-all).
const autostartRegistryKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// installAutostart registers exePath (normally the result of
// os.Executable(), i.e. THIS running gpsync-tray.exe) to launch
// automatically at the next Windows sign-in.
func installAutostart(exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(engine.AutostartValueName, engine.AutostartCommand(exePath))
}

// uninstallAutostart removes the Run-key entry. Not an error if it's
// already gone -- toggling this off twice, or off when it was never on,
// should both just succeed quietly.
func uninstallAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(engine.AutostartValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// autostartInstalled reports whether the Run-key entry currently exists --
// read fresh on every Status poll rather than cached/mirrored into
// config.toml, so it can never drift from reality (e.g. the user removing
// it by hand via Windows' own Settings > Startup Apps page).
func autostartInstalled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(engine.AutostartValueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// runAutostartFlag handles gpsync-tray --install-autostart /
// --uninstall-autostart -- both scriptable one-shots for the same
// install/uninstall path the Status page's toggle button also uses.
func runAutostartFlag(install bool) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own executable path: %w", err)
	}
	if install {
		if err := installAutostart(exePath); err != nil {
			return err
		}
		msg := fmt.Sprintf("Autostart installed: %s will launch automatically at your next Windows sign-in.", appName)
		fmt.Println(msg)
		log.Println(msg)
		return nil
	}
	if err := uninstallAutostart(); err != nil {
		return err
	}
	msg := "Autostart removed."
	fmt.Println(msg)
	log.Println(msg)
	return nil
}
