//go:build windows

package main

import (
	"os"
	"syscall"
)

const enable_virtual_terminal_processing = 0x0004

// enable_ansi turns on escape-code handling in the Windows console, which the
// dashboard needs to move the cursor and redraw in place. It reports false
// when stdout is not a console (e.g. redirected to a file).
func enable_ansi() bool {
	handle := syscall.Handle(os.Stdout.Fd())

	var mode uint32
	if err := syscall.GetConsoleMode(handle, &mode); err != nil {
		return false
	}

	set_console_mode := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
	r, _, _ := set_console_mode.Call(uintptr(handle), uintptr(mode|enable_virtual_terminal_processing))
	return r != 0
}
