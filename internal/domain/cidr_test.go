package domain

import (
	"net/netip"
	"testing"
)

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckVPCCIDR(t *testing.T) {
	cases := []struct {
		cidr string
		ok   bool
	}{
		{"10.0.0.0/16", true},
		{"192.168.0.0/16", true},
		{"172.16.0.0/28", true},
		{"203.0.113.0/24", true},
		{"10.0.0.1/16", false},
		{"10.0.0.0/15", false},
		{"10.0.0.0/29", false},
		{"127.0.0.0/16", false},
		{"169.254.0.0/16", false},
		{"224.0.0.0/16", false},
		{"0.0.0.0/16", false},
		{"::/16", false},
		{"not-a-cidr", false},
	}
	for _, tc := range cases {
		p, err := netip.ParsePrefix(tc.cidr)
		if err != nil {
			if tc.ok {
				t.Fatalf("parse %s: %v", tc.cidr, err)
			}
			continue
		}
		err = CheckVPCCIDR(p)
		if tc.ok && err != nil {
			t.Fatalf("CheckVPCCIDR(%s) = %v", tc.cidr, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("CheckVPCCIDR(%s) accepted", tc.cidr)
		}
	}
}

func TestCheckSubnetCIDR(t *testing.T) {
	vpc := mustPrefix(t, "10.0.0.0/16")
	if err := CheckSubnetCIDR(vpc, mustPrefix(t, "10.0.1.0/24")); err != nil {
		t.Fatal(err)
	}
	if err := CheckSubnetCIDR(vpc, vpc); err != nil {
		t.Fatal("subnet may equal the vpc cidr")
	}
	if err := CheckSubnetCIDR(vpc, mustPrefix(t, "10.1.0.0/24")); err == nil {
		t.Fatal("subnet outside the vpc was accepted")
	}
	if err := CheckSubnetCIDR(vpc, mustPrefix(t, "10.0.0.0/29")); err == nil {
		t.Fatal("/29 subnet was accepted")
	}
	if err := CheckSubnetCIDR(vpc, mustPrefix(t, "10.0.1.1/24")); err == nil {
		t.Fatal("non-canonical subnet was accepted")
	}
}

func TestAssignable(t *testing.T) {
	sub := mustPrefix(t, "10.0.1.0/24")
	for _, addr := range []string{"10.0.1.0", "10.0.1.1", "10.0.1.2", "10.0.1.3", "10.0.1.255"} {
		if Assignable(sub, netip.MustParseAddr(addr)) {
			t.Fatalf("%s is reserved", addr)
		}
	}
	if !Assignable(sub, netip.MustParseAddr("10.0.1.4")) {
		t.Fatal("10.0.1.4 should be usable")
	}
	if Assignable(sub, netip.MustParseAddr("10.0.2.4")) {
		t.Fatal("address outside the subnet was usable")
	}
}

func TestNextAddress(t *testing.T) {
	pool := mustPrefix(t, "198.51.100.0/28")
	taken := map[netip.Addr]bool{netip.MustParseAddr("198.51.100.1"): true}
	avoid := []netip.Prefix{mustPrefix(t, "198.51.100.0/30")}
	got, ok := NextAddress(pool, func(a netip.Addr) bool { return taken[a] }, avoid)
	if !ok || got != netip.MustParseAddr("198.51.100.4") {
		t.Fatalf("got %v ok %v, want 198.51.100.4", got, ok)
	}
	if _, ok := NextAddress(pool, func(netip.Addr) bool { return true }, nil); ok {
		t.Fatal("exhausted pool returned an address")
	}
}

func TestPeerCIDRIsNotVPCCIDR(t *testing.T) {
	peer := mustPrefix(t, "0.0.0.0/0")
	if err := CheckVPCCIDR(peer); err == nil {
		t.Fatal("0.0.0.0/0 is not a vpc cidr")
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 80, ToPort: 80, Peer: Peer{CIDR: peer}}
	if err := CheckRule(rule); err != nil {
		t.Fatal(err)
	}
}
