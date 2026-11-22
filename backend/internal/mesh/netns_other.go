//go:build !linux

package mesh

import (
	"errors"
	"net"
	"net/netip"
)

var errNoNetns = errors.New("mesh capture needs Linux network namespaces")

type netns struct{}

func enterNetns(int) (*netns, error)                          { return nil, errNoNetns }
func (*netns) listen(string) (net.Listener, error)            { return nil, errNoNetns }
func (*netns) dial(string) (net.Conn, error)                  { return nil, errNoNetns }
func (*netns) close()                                         {}
func (*netns) capture(string) error                           { return errNoNetns }
func (*netns) dropped() (int64, error)                        { return 0, errNoNetns }
func (*netns) routeUDP() error                                { return errNoNetns }
func (*netns) listenUDP(string) (*net.UDPConn, error)         { return nil, errNoNetns }
func (*netns) replyAs(netip.AddrPort) (net.PacketConn, error) { return nil, errNoNetns }
func originalDst(net.Conn) (netip.AddrPort, error)            { return netip.AddrPort{}, errNoNetns }

func readUDP(*net.UDPConn, []byte, []byte) (int, netip.AddrPort, netip.AddrPort, error) {
	return 0, netip.AddrPort{}, netip.AddrPort{}, errNoNetns
}
