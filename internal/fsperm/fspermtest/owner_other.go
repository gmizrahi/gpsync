//go:build !windows

package fspermtest

import (
	"fmt"
	"os"
)

func ownerOnlyProblem(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf("mode %#o grants group or other access", perm)
	}
	return ""
}
