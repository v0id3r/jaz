//go:build !windows

package sandbox

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

// fakePodman runs `podman run` as `cat` with a marker on stderr, or with -t as
// a terminal probe, and turns `podman stop`/`kill` into a SIGTERM to that
// process, recording the call.
func fakePodman(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "podman")
	script := "#!/bin/sh\n" +
		"case \"$1 $*\" in\n" +
		"run\\ *\\ -t\\ *) stty size; read line; stty size; echo \"got:$line\" ;;\n" +
		"run*) echo $$ > " + dir + "/pid; echo started >&2; exec cat ;;\n" +
		"stop*|kill*) echo \"$*\" >> " + dir + "/calls; kill $(cat " + dir + "/pid) ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRunner(t *testing.T) (*runner, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := RunnerConfig{
		Podman: fakePodman(t, dir), Image: "image", Network: "net", StopTimeout: time.Second,
		Mounts:   []PathMapping{{Container: "/jaz", Host: dir}},
		Writable: []string{"/jaz/work"},
	}
	return &runner{cfg: cfg, log: log.New(io.Discard), active: map[string]struct{}{}}, dir
}

func serveOnPipe(r *runner) (net.Conn, <-chan struct{}) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		r.serve(server)
		close(done)
	}()
	return client, done
}

func TestAttachRelaysStdioAndExitStatus(t *testing.T) {
	r, _ := testRunner(t)
	client, _ := serveOnPipe(r)
	var stdout, stderr bytes.Buffer
	code, err := attach(client, Request{Args: []string{"agent"}, Dir: "/jaz/work"}, strings.NewReader("initialize\n"), &stdout, &stderr, make(chan os.Signal), nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || stdout.String() != "initialize\n" || !strings.Contains(stderr.String(), "started") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestRunnerRejectsWritablePathOutsideRoots(t *testing.T) {
	r, _ := testRunner(t)
	client, _ := serveOnPipe(r)
	var stderr bytes.Buffer
	code, err := attach(client, Request{Args: []string{"agent"}, Dir: "/jaz"}, strings.NewReader(""), io.Discard, &stderr, make(chan os.Signal), nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 126 || !strings.Contains(stderr.String(), "not under a writable root") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunnerStopsContainerWhenClientDisappears(t *testing.T) {
	r, dir := testRunner(t)
	client, done := serveOnPipe(r)
	out := &frameWriter{w: client}
	if err := out.write(frameRequest, []byte(`{"args":["agent"],"dir":"/jaz/work"}`)); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, client) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container did not start")
		}
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner kept serving after the client disappeared")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.HasPrefix(string(calls), "stop --time 1 jaz-") {
		t.Fatalf("podman calls = %q, want a stop of the container", calls)
	}
}

func TestAttachRunsOnTerminalAndFollowsResize(t *testing.T) {
	r, _ := testRunner(t)
	client, _ := serveOnPipe(r)
	stdin, input := io.Pipe()
	resizes := make(chan [2]uint16, 1)
	stdout := &watchedBuffer{want: "30 100", seen: make(chan struct{})}
	go func() {
		<-stdout.seen
		resizes <- [2]uint16{40, 120}
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(input, "hello\n")
	}()
	code, err := attach(client, Request{Args: []string{"sh"}, Dir: "/jaz/work", TTY: true, Rows: 30, Cols: 100}, stdin, stdout, io.Discard, make(chan os.Signal), resizes)
	if err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if code != 0 || !strings.Contains(out, "40 120") || !strings.Contains(out, "got:hello") {
		t.Fatalf("code = %d, stdout = %q", code, out)
	}
}

// watchedBuffer closes seen once its contents include want.
type watchedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func (w *watchedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *watchedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
