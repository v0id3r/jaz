package mesh

import (
	"net/netip"
	"strings"
	"testing"
)

func TestCaptureRules(t *testing.T) {
	jaz := []netip.AddrPort{netip.MustParseAddrPort("10.216.92.10:5299")}
	direct := []netip.AddrPort{netip.MustParseAddrPort("10.216.93.97:4010")}
	dns := []netip.Addr{netip.MustParseAddr("10.216.94.1")}

	only := captureRules(jaz, nil, nil, 15001, false)
	if !strings.Contains(only, "ip daddr 10.216.92.10 tcp dport 5299 redirect to :15001") || strings.Contains(only, "policy drop") {
		t.Fatalf("without egress only Jaz is captured and nothing is dropped:\n%s", only)
	}

	all := captureRules(jaz, direct, dns, 15001, true)
	for _, want := range []string{
		"meta mark 0x6a7a return",
		"ip daddr 127.0.0.0/8 return",
		"ip daddr 10.216.93.97 tcp dport 4010 return",
		"ip daddr 10.216.94.1 tcp dport 53 return",
		"meta nfproto ipv4 meta l4proto tcp redirect to :15001",
		"policy drop;",
		"meta mark 0x6a7a accept",
		"ct status dnat accept",
		"ip daddr 10.216.94.1 meta l4proto { tcp, udp } th dport 53 accept",
		"counter name dropped drop",
		"type route hook output priority mangle;",
		"ip daddr 10.216.94.1 udp dport 53 return",
		"meta nfproto ipv4 meta l4proto udp meta mark set 0x6a75",
		"iifname \"lo\" meta mark 0x6a75 meta nfproto ipv4 meta l4proto udp tproxy ip to 127.0.0.1:15001",
		"meta mark 0x6a75 meta l4proto udp accept",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	// Jaz is captured by the catch-all; the gateway is chosen by destination.
	if strings.Contains(all, "10.216.92.10") {
		t.Errorf("Jaz needs no rule of its own with egress:\n%s", all)
	}
	// Exemptions come before the catch-all redirect.
	if strings.Index(all, "tcp dport 4010 return") > strings.Index(all, "redirect to") {
		t.Errorf("direct after the redirect:\n%s", all)
	}
	if strings.Index(all, "udp dport 53 return") > strings.Index(all, "meta mark set") {
		t.Errorf("DNS after the UDP mark:\n%s", all)
	}
}
