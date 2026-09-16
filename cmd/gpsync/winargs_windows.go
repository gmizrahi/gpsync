//go:build windows

package main

import "golang.org/x/sys/windows"

// rawCommandLine returns the process's command line exactly as Windows
// received it, before any C-runtime-style splitting -- the input
// repairWindowsArgs needs to recover paths that end in a backslash.
func rawCommandLine() string {
	return windows.UTF16PtrToString(windows.GetCommandLine())
}
