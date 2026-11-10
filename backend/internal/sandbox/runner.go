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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/charmbracelet/log"
	"github.com/creack/pty"

	"github.com/wins/jaz/backend/internal/mesh"
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
	if cfg.Identity.enabled() {
		if r.identities, err = newIdentities(cfg.Identity, logger); err != nil {
			logger.Error("session identities", "error", err)
			return 1
		}
		defer r.identities.conn.Close()
		r.identities.removeStale()
	}
	if cfg.Mesh.Enabled() {
		if r.mesh, err = mesh.NewOutbound(cfg.Mesh, logger); err != nil {
			logger.Error("mesh", "error", err)
			return 1
		}
		binary, err := os.Executable()
		if err == nil {
			var stopHooks func()
			if stopHooks, err = r.mesh.Start(binary); err == nil {
				defer stopHooks()
			}
		}
		if err != nil {
			logger.Error("mesh hooks", "error", err)
			return 1
		}
	}
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
	cfg        RunnerConfig
	log        *log.Logger
	identities *identities
	mesh       *mesh.Outbound
	mu         sync.Mutex
	active     map[string]struct{}
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
	if err == nil && req.IdentityLease {
		r.holdIdentity(conn, out, req.Session)
		return
	}
	if err == nil {
		args, env, err = r.cfg.runArgs(name, req)
	}
	if err == nil && req.Session != "" && r.identities != nil {
		var release func()
		if release, err = r.identities.acquire(context.Background(), req.Session, false); err == nil {
			defer release()
		}
	}
	if err == nil && req.Session != "" && r.mesh != nil {
		// podman's createRuntime hook reports the container, and the runner
		// captures its network before the session's command starts.
		global, annotations := r.mesh.PodmanArgs(name, req.Session)
		args = slices.Concat(global, args[:1], annotations, args[1:])
		defer r.mesh.Expect(name, req.Session)()
	}
	var proc *process
	if err == nil {
		cmd := exec.Command(r.cfg.Podman, args...)
		cmd.Env = env
		if req.TTY {
			proc, err = startTTY(cmd, req.Rows, req.Cols)
		} else {
			proc, err = startPipes(cmd)
		}
	}
	if err != nil {
		r.log.Warn("rejected launch", "error", err)
		_ = out.write(frameStderr, []byte("jaz runner: "+err.Error()+"\n"))
		_ = out.writeInt(frameExit, 126)
		return
	}
	r.track(name, true)
	defer r.track(name, false)
	r.log.Info("started", "container", name, "command", req.Args[0], "dir", req.Dir, "tty", req.TTY)

	var output sync.WaitGroup
	for kind, stream := range proc.outputs {
		output.Add(1)
		go func() {
			defer output.Done()
			_ = pump(out, kind, stream)
		}()
	}
	exited := make(chan struct{})
	go r.relayInput(conn, proc, name, exited)
	output.Wait()
	code := exitCode(proc.wait())
	close(exited)
	r.log.Info("exited", "container", name, "code", code)
	_ = out.writeInt(frameExit, code)
}

// process is a started podman client and the streams that reach it.
type process struct {
	stdin   io.WriteCloser
	outputs map[byte]io.Reader
	resize  func(rows, cols uint16)
	wait    func() error
}

func startPipes(cmd *exec.Cmd) (*process, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{
		stdin:   stdin,
		outputs: map[byte]io.Reader{frameStdout: stdout, frameStderr: stderr},
		resize:  func(uint16, uint16) {},
		wait:    cmd.Wait,
	}, nil
}

// startTTY runs podman on a pseudo-terminal, so `-t` gives the container a
// terminal that follows this one's size; its output is a single stream.
func startTTY(cmd *exec.Cmd, rows, cols uint16) (*process, error) {
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	return &process{
		stdin:   nopCloser{ptmx},
		outputs: map[byte]io.Reader{frameStdout: ptmx},
		resize: func(rows, cols uint16) {
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
		},
		wait: func() error {
			err := cmd.Wait()
			_ = ptmx.Close()
			return err
		},
	}, nil
}

// nopCloser keeps a terminal open when the client's input ends; a terminal
// has no end of input, only a hangup when the process exits.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// relayInput feeds client frames to the container until the client goes away;
// a client that disappears while the container runs gets it stopped.
func (r *runner) relayInput(conn net.Conn, proc *process, name string, exited <-chan struct{}) {
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
			_, _ = proc.stdin.Write(payload)
		case frameCloseStdin:
			_ = proc.stdin.Close()
		case frameSignal:
			if number, err := frameInt(payload); err == nil {
				r.podman("kill", "--signal", strconv.Itoa(number), name)
			}
		case frameResize:
			if rows, cols, ok := frameSize(payload); ok {
				proc.resize(rows, cols)
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
