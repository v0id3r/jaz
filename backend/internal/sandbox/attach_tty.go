//go:build !windows

package sandbox

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
)

// openTTY switches the terminal on stdin to raw mode, so the remote terminal
// does all line editing, and reports its size and later resizes.
func openTTY() (rows, cols uint16, resizes <-chan [2]uint16, restore func(), err error) {
	fd := os.Stdin.Fd()
	width, height, err := term.GetSize(fd)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	sizes := make(chan [2]uint16, 1)
	go func() {
		for range winch {
			if width, height, err := term.GetSize(fd); err == nil {
				sizes <- [2]uint16{uint16(height), uint16(width)}
			}
		}
	}()
	return uint16(height), uint16(width), sizes, func() {
		signal.Stop(winch)
		_ = term.Restore(fd, state)
	}, nil
}
