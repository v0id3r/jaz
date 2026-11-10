//go:build !linux

package mesh

import (
	"errors"
	"net"
	"net/netip"
)

var errNoNetns = errors.New("mesh capture needs Linux network namespaces")

type netns struct{}

func enterNetns(int) (*netns, error)               { return nil, errNoNetns }
func (*netns) listen(string) (net.Listener, error) { return nil, errNoNetns }
func (*netns) dial(string) (net.Conn, error)       { return nil, errNoNetns }
func (*netns) close()                              {}
func (*netns) capture([]netip.AddrPort, int) error { return errNoNetns }
func originalDst(net.Conn) (netip.AddrPort, error) { return netip.AddrPort{}, errNoNetns }
