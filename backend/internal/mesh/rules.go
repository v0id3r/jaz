package mesh

import (
	"fmt"
	"net/netip"
	"strings"
)

const (
	// tunnelMark is the socket mark ("jz") on the runner's own sockets in a
	// session's namespace: tunnels and UDP replies.
	tunnelMark = 0x6a7a
	// udpMark ("ju") marks captured UDP; the routing rule for it sends it to
	// table udpTable, which delivers everything locally, to the TPROXY socket.
	udpMark  = 0x6a75
	udpTable = udpMark
)

// captureRules is the nftables ruleset a session's namespace gets.
//
// Without egress only the connections to capture go to the capture port, as
// before. With egress every new IPv4 TCP connection goes there, save those to
// loopback, to direct and to the DNS resolvers on port 53, and IPv4 UDP but
// DNS to the resolvers and loopback is marked, routed back in through
// loopback and taken by TPROXY on the same port. The namespace sends nothing
// else: DNS to the resolvers, direct, loopback and replies on existing
// connections pass, the rest (IPv6, ICMP, other protocols) is counted in
// "dropped" and dropped. The runner's own sockets carry tunnelMark and pass.
func captureRules(capture, direct []netip.AddrPort, dns []netip.Addr, port int, egress bool) string {
	var b strings.Builder
	if !egress {
		b.WriteString("table ip jaz_mesh {\n chain output {\n  type nat hook output priority dstnat; policy accept;\n")
		fmt.Fprintf(&b, "  meta mark %#x return\n", tunnelMark)
		for _, dst := range capture {
			fmt.Fprintf(&b, "  ip daddr %s tcp dport %d redirect to :%d\n", dst.Addr(), dst.Port(), port)
		}
		b.WriteString(" }\n}\n")
		return b.String()
	}
	b.WriteString("table inet jaz_mesh {\n counter dropped { packets 0 bytes 0 }\n")
	b.WriteString(" chain nat_output {\n  type nat hook output priority dstnat; policy accept;\n")
	fmt.Fprintf(&b, "  meta mark %#x return\n", tunnelMark)
	b.WriteString("  ip daddr 127.0.0.0/8 return\n")
	for _, dst := range direct {
		fmt.Fprintf(&b, "  ip daddr %s tcp dport %d return\n", dst.Addr(), dst.Port())
	}
	for _, r := range dns {
		fmt.Fprintf(&b, "  ip daddr %s tcp dport 53 return\n", r)
	}
	fmt.Fprintf(&b, "  meta nfproto ipv4 meta l4proto tcp redirect to :%d\n }\n", port)
	// A route chain reroutes a packet whose mark it changed.
	b.WriteString(" chain udp_output {\n  type route hook output priority mangle; policy accept;\n")
	fmt.Fprintf(&b, "  meta mark %#x return\n", tunnelMark)
	b.WriteString("  ip daddr 127.0.0.0/8 return\n")
	for _, r := range dns {
		fmt.Fprintf(&b, "  ip daddr %s udp dport 53 return\n", r)
	}
	fmt.Fprintf(&b, "  meta nfproto ipv4 meta l4proto udp meta mark set %#x\n }\n", udpMark)
	b.WriteString(" chain udp_prerouting {\n  type filter hook prerouting priority mangle; policy accept;\n")
	fmt.Fprintf(&b, "  iifname \"lo\" meta mark %#x meta nfproto ipv4 meta l4proto udp tproxy ip to 127.0.0.1:%d\n }\n", udpMark, port)
	b.WriteString(" chain filter_output {\n  type filter hook output priority filter; policy drop;\n")
	fmt.Fprintf(&b, "  meta mark %#x accept\n", tunnelMark)
	fmt.Fprintf(&b, "  meta mark %#x meta l4proto udp accept\n", udpMark)
	b.WriteString("  oifname \"lo\" accept\n  ct state established,related accept\n  ct status dnat accept\n")
	for _, dst := range direct {
		fmt.Fprintf(&b, "  ip daddr %s tcp dport %d accept\n", dst.Addr(), dst.Port())
	}
	for _, r := range dns {
		fmt.Fprintf(&b, "  ip daddr %s meta l4proto { tcp, udp } th dport 53 accept\n", r)
	}
	b.WriteString("  counter name dropped drop\n }\n}\n")
	return b.String()
}
