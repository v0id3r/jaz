package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// holdIdentity serves an identity lease: the session's delegated entry lives
// until the client goes away. The exit frame answers the request, 0 when the
// identity is registered.
func (r *runner) holdIdentity(conn net.Conn, out *frameWriter, session string) {
	if r.identities == nil {
		_ = out.write(frameStderr, []byte("jaz runner: session identities are not configured\n"))
		_ = out.writeInt(frameExit, 126)
		return
	}
	release, err := r.identities.acquire(context.Background(), session, true)
	if err != nil {
		r.log.Warn("rejected identity lease", "session", session, "error", err)
		_ = out.write(frameStderr, []byte("jaz runner: "+err.Error()+"\n"))
		_ = out.writeInt(frameExit, 126)
		return
	}
	defer release()
	if err := out.writeInt(frameExit, 0); err != nil {
		return
	}
	// The client sends nothing more; EOF ends the lease.
	_, _ = io.Copy(io.Discard, conn)
}

// HoldIdentity asks the runner at socket to keep the session's identity
// registered for a local process; release ends the lease.
func HoldIdentity(socket, session string) (release func(), err error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	data, err := json.Marshal(Request{Session: session, IdentityLease: true})
	if err != nil {
		return nil, err
	}
	out := &frameWriter{w: conn}
	if err := out.write(frameRequest, data); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var stderr strings.Builder
	for {
		kind, payload, err := readFrame(conn)
		if err != nil {
			return nil, fmt.Errorf("identity lease: %w", err)
		}
		switch kind {
		case frameStderr:
			stderr.Write(payload)
		case frameExit:
			code, err := frameInt(payload)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, errors.New(strings.TrimSpace(stderr.String()))
			}
			_ = conn.SetReadDeadline(time.Time{})
			return func() { _ = conn.Close() }, nil
		}
	}
}
