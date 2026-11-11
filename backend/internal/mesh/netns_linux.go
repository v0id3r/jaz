//go:build linux

package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// routeUDP sends packets with udpMark to table udpTable, where every address
// is local: marked UDP comes back in through loopback for TPROXY.
func (n *netns) routeUDP() error {
	mark, table := fmt.Sprintf("%#x", udpMark), fmt.Sprint(udpTable)
	if _, err := n.run("ip", "", "-4", "rule", "add", "fwmark", mark, "lookup", table); err != nil {
		return err
	}
	_, err := n.run("ip", "", "-4", "route", "add", "local", "0.0.0.0/0", "dev", "lo", "table", table)
	return err
}

// listenUDP opens the TPROXY socket at addr: it receives the datagrams of
// any destination that TPROXY hands it and tells each one's destination.
func (n *netns) listenUDP(addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: sockopts(func(fd int) error {
		if err := unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_TRANSPARENT, 1); err != nil {
			return err
		}
		return unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_RECVORIGDSTADDR, 1)
	})}
	var conn net.PacketConn
	var err error
	if derr := n.do(func() { conn, err = lc.ListenPacket(context.Background(), "udp4", addr) }); derr != nil {
		return nil, derr
	}
	if err != nil {
		return nil, err
	}
	return conn.(*net.UDPConn), nil
}

// replyAs opens a socket that sends from dst, an address not of the
// namespace, so that replies reach a session as from where it sent.
func (n *netns) replyAs(dst netip.AddrPort) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: sockopts(func(fd int) error {
		for _, opt := range [][3]int{{unix.SOL_IP, unix.IP_TRANSPARENT, 1}, {unix.SOL_SOCKET, unix.SO_REUSEADDR, 1}, {unix.SOL_SOCKET, unix.SO_MARK, tunnelMark}} {
			if err := unix.SetsockoptInt(fd, opt[0], opt[1], opt[2]); err != nil {
				return err
			}
		}
		return nil
	})}
	var conn net.PacketConn
	var err error
	if derr := n.do(func() { conn, err = lc.ListenPacket(context.Background(), "udp4", dst.String()) }); derr != nil {
		return nil, derr
	}
	return conn, err
}

func sockopts(set func(fd int) error) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var serr error
		if err := raw.Control(func(fd uintptr) { serr = set(int(fd)) }); err != nil {
			return err
		}
		return serr
	}
}

// readUDP reads a datagram from a TPROXY socket with its source and
// original destination.
func readUDP(conn *net.UDPConn, buf, oob []byte) (n int, src, dst netip.AddrPort, err error) {
	n, oobn, _, src, err := conn.ReadMsgUDPAddrPort(buf, oob)
	if err != nil {
		return 0, src, dst, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return 0, src, dst, err
	}
	for _, m := range msgs {
		// A sockaddr_in: family, port (big-endian), address.
		if m.Header.Level == unix.SOL_IP && m.Header.Type == unix.IP_ORIGDSTADDR && len(m.Data) >= 8 {
			dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(m.Data[4:8])), uint16(m.Data[2])<<8|uint16(m.Data[3]))
			return n, netip.AddrPortFrom(src.Addr().Unmap(), src.Port()), dst, nil
		}
	}
	return 0, src, dst, errors.New("datagram without its original destination")
}

func (n *netns) nft(stdin string, args ...string) ([]byte, error) {
	return n.run("nft", stdin, args...)
}

// run runs a program from the namespace's thread, so the child starts in the
// namespace; it works even after the container's process is gone.
func (n *netns) run(name, stdin string, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	var err error
	derr := n.do(func() {
		cmd := exec.Command(name, args...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err = cmd.Run()
	})
	if derr != nil {
		return nil, derr
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errOut.String()))
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
