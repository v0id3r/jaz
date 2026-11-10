//go:build linux

package mesh

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"

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
	if derr := n.do(func() { conn, err = net.DialTimeout("tcp", addr, dialTimeout) }); derr != nil {
		return nil, derr
	}
	return conn, err
}

func (n *netns) close() { close(n.done) }

// capture redirects the namespace's connections to the destinations to the
// capture port on its loopback; only the runner, not the session, can change
// these rules, as sessions hold no network capabilities.
func (n *netns) capture(destinations []netip.AddrPort, port int) error {
	var rules strings.Builder
	rules.WriteString("table ip jaz_mesh {\n chain output {\n  type nat hook output priority dstnat; policy accept;\n")
	for _, dst := range destinations {
		fmt.Fprintf(&rules, "  ip daddr %s tcp dport %d redirect to :%d\n", dst.Addr(), dst.Port(), port)
	}
	rules.WriteString(" }\n}\n")
	cmd := exec.Command("nsenter", fmt.Sprintf("--net=/proc/%d/ns/net", n.pid), "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules.String())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
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
