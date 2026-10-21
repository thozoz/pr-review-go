//go:build !linux

package main

import (
	"fmt"
	"os"
	"runtime"
)

// runLandlockRun is the non-Linux stub: Landlock is a Linux-only LSM, so the
// rank is unavailable. It deliberately does not parse flags, touch the
// filesystem, or restrict anything — the typed 70 exit is the whole contract,
// and the other subcommands keep compiling and behaving unchanged.
func runLandlockRun(_ []string) int {
	code, reason := landlockUnsupportedExit(runtime.GOOS)
	fmt.Fprintln(os.Stderr, reason)
	return code
}
