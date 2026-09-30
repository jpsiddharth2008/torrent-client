//go:build !windows

package main

import "os"

// enable_ansi reports whether stdout is a terminal; Unix terminals handle
// escape codes natively.
func enable_ansi() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
