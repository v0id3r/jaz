package sandbox

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const attachUsage = "usage: jaz sandbox-attach --socket path [--tty] [--session id] [--ro path]... [--rw path]... -- command [args...]"

// Attach runs a command in a runner-managed container, relaying stdio and
// termination signals, and returns the command's exit status. The command
// inherits this process's environment and working directory.
func Attach(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "runner socket path")
	var req Request
	fs.Var((*pathList)(&req.ReadOnly), "ro", "path the command reads (repeatable)")
	fs.Var((*pathList)(&req.ReadWrite), "rw", "path the command writes (repeatable)")
	fs.BoolVar(&req.TTY, "tty", false, "run the command on a terminal sized like stdin's")
	fs.StringVar(&req.Session, "session", "", "Jaz session the command works for")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	req.Args = fs.Args()
	if *socket == "" || len(req.Args) == 0 {
		fmt.Fprintln(stderr, attachUsage)
		return 2
	}
	req.Env = os.Environ()
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "sandbox-attach:", err)
		return 1
	}
	req.Dir = dir
	var resizes <-chan [2]uint16
	if req.TTY {
		var restore func()
		req.Rows, req.Cols, resizes, restore, err = openTTY()
		if err != nil {
			fmt.Fprintln(stderr, "sandbox-attach:", err)
			return 1
		}
		defer restore()
	}
	conn, err := net.Dial("unix", *socket)
	if err != nil {
		fmt.Fprintln(stderr, "sandbox-attach:", err)
		return 1
	}
	defer conn.Close()
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	code, err := attach(conn, req, stdin, stdout, stderr, signals, resizes)
	if err != nil {
		fmt.Fprintln(stderr, "sandbox-attach:", err)
		return 1
	}
	return code
}

func attach(conn io.ReadWriter, req Request, stdin io.Reader, stdout, stderr io.Writer, signals <-chan os.Signal, resizes <-chan [2]uint16) (int, error) {
	out := &frameWriter{w: conn}
	data, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	if err := out.write(frameRequest, data); err != nil {
		return 0, err
	}
	if resizes != nil {
		go func() {
			for size := range resizes {
				_ = out.write(frameResize, resizePayload(size[0], size[1]))
			}
		}()
	}
	go func() {
		if pump(out, frameStdin, stdin) == nil {
			_ = out.write(frameCloseStdin, nil)
		}
	}()
	go func() {
		for sig := range signals {
			if number, ok := sig.(syscall.Signal); ok {
				_ = out.writeInt(frameSignal, int(number))
			}
		}
	}()
	for {
		kind, payload, err := readFrame(conn)
		if err != nil {
			return 0, fmt.Errorf("runner connection: %w", err)
		}
		switch kind {
		case frameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 0, err
			}
		case frameStderr:
			_, _ = stderr.Write(payload)
		case frameExit:
			return frameInt(payload)
		}
	}
}

type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }

func (p *pathList) Set(value string) error {
	*p = append(*p, value)
	return nil
}
