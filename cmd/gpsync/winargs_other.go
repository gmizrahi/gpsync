//go:build !windows

package main

// rawCommandLine has nothing to recover outside Windows: POSIX shells hand
// the program an already-split argument vector, with no quote-escaping
// ambiguity for repairWindowsArgs to undo.
func rawCommandLine() string { return "" }
