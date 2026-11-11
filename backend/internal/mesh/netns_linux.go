//go:build linux

package mesh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// netns makes sockets inside another network namespace: a goroutine locked to
// an OS thread that joined the namespace. The thread never leaves it; Go
// retires a thread whose goroutine exits while still locked.
type netns struct {
	pid   int
	calls chan func()
	done  chan struct{}
}

func enterNetns(pid int) (*netns, error) {
	fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/net", pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("network namespace of pid %d: %w", pid, err)
	}
	n := &netns{pid: pid, calls: make(chan func()), done: make(chan struct{})}
	entered := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		err := unix.Setns(fd, unix.CLONE_NEWNET)
		unix.Close(fd)
		entered <- err
		if err != nil {
			return
		}
		for {
			select {
			case call := <-n.calls:
				call()
			case <-n.done:
				return
			}
		}
	}()
	if err := <-entered; err != nil {
		return nil, fmt.Errorf("enter network namespace of pid %d: %w", pid, err)
	}
	return n, nil
}

func (n *netns) do(call func()) error {
	finished := make(chan struct{})
	select {
	case n.calls <- func() { call(); close(finished) }:
	case <-n.done:
		return net.ErrClosed
	}
	<-finished
	return nil
}

func (n *netns) listen(addr string) (net.Listener, error) {
	var ln net.Listener
	var err error
	if derr := n.do(func() { ln, err = net.Listen("tcp", addr) }); derr != nil {
		return nil, derr
	}
	return ln, err
}

func (n *netns) dial(addr string) (net.Conn, error) {
	var conn net.Conn
	var err error
	// The mark exempts the tunnel itself from the capture rules, for tunnels
	// to a captured destination such as Jaz.
	dialer := net.Dialer{Timeout: dialTimeout, Control: func(_, _ string, raw syscall.RawConn) error {
		var serr error
		if err := raw.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, tunnelMark) }); err != nil {
			return err
		}
		return serr
	}}
	if derr := n.do(func() { conn, err = dialer.Dial("tcp", addr) }); derr != nil {
		return nil, derr
	}
	return conn, err
}

func (n *netns) close() { close(n.done) }

// capture loads rules, from captureRules, into the namespace; only the runner,
// not the session, can change them, as sessions hold no network capabilities.
func (n *netns) capture(rules string) error {
	_, err := n.nft(rules, "-f", "-")
	return err
}

// dropped reads how many packets the namespace's rules dropped.
func (n *netns) dropped() (int64, error) {
	out, err := n.nft("", "-j", "list", "counter", "inet", "jaz_mesh", "dropped")
	if err != nil {
		return 0, err
	}
	var listing struct {
		Nftables []struct {
			Counter *struct {
				Packets int64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return 0, err
	}
	for _, item := range listing.Nftables {
		if item.Counter != nil {
			return item.Counter.Packets, nil
		}
	}
	return 0, fmt.Errorf("nft: no dropped counter")
}

// nft runs nft from the namespace's thread, so the child starts in the
// namespace; it works even after the container's process is gone.
func (n *netns) nft(stdin string, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	var err error
	derr := n.do(func() {
		cmd := exec.Command("nft", args...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err = cmd.Run()
	})
	if derr != nil {
		return nil, derr
	}
	if err != nil {
		return nil, fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// originalDst is where a redirected connection was headed (SO_ORIGINAL_DST).
func originalDst(conn net.Conn) (netip.AddrPort, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("not a TCP connection")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var serr error
	err = raw.Control(func(fd uintptr) {
		// The kernel returns a sockaddr_in in the 16 bytes of an IPv6 mreq.
		mreq, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			serr = err
			return
		}
		sa := mreq.Multiaddr
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(sa[4:8])), uint16(sa[2])<<8|uint16(sa[3]))
	})
	if err == nil {
		err = serr
	}
	return dst, err
}
