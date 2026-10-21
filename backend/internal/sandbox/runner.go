package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/charmbracelet/log"
)

// ServeRunner is `jaz runner`: it accepts process launches from Jaz on a unix
// socket and runs each one as a podman container.
func ServeRunner(args []string) int {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/jaz/runner.yaml", "runner config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.Default().WithPrefix("runner")
	cfg, err := loadRunnerConfig(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		return 1
	}
	r := &runner{cfg: cfg, log: logger, active: map[string]struct{}{}}
	r.removeOrphans()
	listener, err := r.listen()
	if err != nil {
		logger.Error("listen", "socket", cfg.Socket, "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	logger.Info("serving", "socket", cfg.Socket, "image", cfg.Image, "network", cfg.Network)
	for {
		conn, err := listener.Accept()
		if err != nil {
			break
		}
		go r.serve(conn)
	}
	r.stopActive()
	return 0
}

type runner struct {
	cfg    RunnerConfig
	log    *log.Logger
	mu     sync.Mutex
	active map[string]struct{}
}

func (r *runner) listen() (net.Listener, error) {
	_ = os.Remove(r.cfg.Socket)
	listener, err := net.Listen("unix", r.cfg.Socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(r.cfg.Socket, 0o660); err != nil {
		listener.Close()
		return nil, err
	}
	if r.cfg.SocketOwner != "" {
		uid, gid, ok := strings.Cut(r.cfg.SocketOwner, ":")
		u, uerr := strconv.Atoi(uid)
		g, gerr := strconv.Atoi(gid)
		if !ok || uerr != nil || gerr != nil {
			listener.Close()
			return nil, fmt.Errorf("socket owner %q is not uid:gid", r.cfg.SocketOwner)
		}
		if err := os.Chown(r.cfg.Socket, u, g); err != nil {
			listener.Close()
			return nil, err
		}
	}
	return listener, nil
}

func (r *runner) serve(conn net.Conn) {
	defer conn.Close()
	out := &frameWriter{w: conn}
	name := "jaz-" + randomHex(6)
	var req Request
	var args, env []string
	kind, payload, err := readFrame(conn)
	if err == nil && kind != frameRequest {
		err = errors.New("expected a request frame")
	}
	if err == nil {
		err = json.Unmarshal(payload, &req)
	}
	if err == nil {
		args, env, err = r.cfg.runArgs(name, req)
	}
	cmd := exec.Command(r.cfg.Podman, args...)
	cmd.Env = env
	var stdin io.WriteCloser
	var stdout, stderr io.ReadCloser
	if err == nil {
		stdin, err = cmd.StdinPipe()
	}
	if err == nil {
		stdout, err = cmd.StdoutPipe()
	}
	if err == nil {
		stderr, err = cmd.StderrPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		r.log.Warn("rejected launch", "error", err)
		_ = out.write(frameStderr, []byte("jaz runner: "+err.Error()+"\n"))
		_ = out.writeInt(frameExit, 126)
		return
	}
	r.track(name, true)
	defer r.track(name, false)
	r.log.Info("started", "container", name, "command", req.Args[0], "dir", req.Dir)

	var output sync.WaitGroup
	output.Add(2)
	go func() {
		defer output.Done()
		_ = pump(out, frameStdout, stdout)
	}()
	go func() {
		defer output.Done()
		_ = pump(out, frameStderr, stderr)
	}()
	exited := make(chan struct{})
	go r.relayInput(conn, stdin, name, exited)
	output.Wait()
	code := exitCode(cmd.Wait())
	close(exited)
	r.log.Info("exited", "container", name, "code", code)
	_ = out.writeInt(frameExit, code)
}

// relayInput feeds client frames to the container until the client goes away;
// a client that disappears while the container runs gets it stopped.
func (r *runner) relayInput(conn net.Conn, stdin io.WriteCloser, name string, exited <-chan struct{}) {
	for {
		kind, payload, err := readFrame(conn)
		if err != nil {
			select {
			case <-exited:
			default:
				r.stop(name)
			}
			return
		}
		switch kind {
		case frameStdin:
			_, _ = stdin.Write(payload)
		case frameCloseStdin:
			_ = stdin.Close()
		case frameSignal:
			if number, err := frameInt(payload); err == nil {
				r.podman("kill", "--signal", strconv.Itoa(number), name)
			}
		}
	}
}

func (r *runner) stop(name string) {
	r.podman("stop", "--time", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())), name)
}

func (r *runner) stopActive() {
	r.mu.Lock()
	names := make([]string, 0, len(r.active))
	for name := range r.active {
		names = append(names, name)
	}
	r.mu.Unlock()
	var stopping sync.WaitGroup
	for _, name := range names {
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			r.stop(name)
		}()
	}
	stopping.Wait()
}

// removeOrphans removes containers left by a runner that exited without
// stopping them; their Jaz clients are gone.
func (r *runner) removeOrphans() {
	out, err := exec.Command(r.cfg.Podman, "ps", "--all", "--quiet", "--filter", "label="+runnerLabel).Output()
	if err != nil {
		r.log.Warn("list orphaned containers", "error", err)
		return
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		r.log.Info("removing orphaned containers", "count", len(ids))
		r.podman(append([]string{"rm", "--force"}, ids...)...)
	}
}

func (r *runner) podman(args ...string) {
	if out, err := exec.Command(r.cfg.Podman, args...).CombinedOutput(); err != nil {
		r.log.Warn("podman", "args", args, "error", err, "output", strings.TrimSpace(string(out)))
	}
}

func (r *runner) track(name string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if active {
		r.active[name] = struct{}{}
	} else {
		delete(r.active, name)
	}
}

func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode()
	case errors.As(err, &exit):
		return 137
	default:
		return 1
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
