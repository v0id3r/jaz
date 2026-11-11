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
func (*netns) capture(string) error                { return errNoNetns }
func (*netns) dropped() (int64, error)             { return 0, errNoNetns }
func originalDst(net.Conn) (netip.AddrPort, error) { return netip.AddrPort{}, errNoNetns }
