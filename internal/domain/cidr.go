package domain

import "net/netip"

// CheckVPCCIDR accepts a canonical global-unicast IPv4 prefix from /16 through /28.
// Global unicast rejects 0.0.0.0/8, loopback, link-local, and multicast.
// It does not reject 240.0.0.0/4. Host bits must already be clear.
func CheckVPCCIDR(p netip.Prefix) error {
	if err := canonicalIPv4(p); err != nil {
		return err
	}
	if p.Bits() < 16 || p.Bits() > 28 {
		return invalid("vpc cidr prefix length must be 16 through 28")
	}
	if !p.Addr().IsGlobalUnicast() {
		return invalid("vpc cidr must be global unicast")
	}
	return nil
}

// CheckSubnetCIDR accepts a canonical IPv4 prefix inside vpc, from the VPC
// prefix length through /28.
func CheckSubnetCIDR(vpc, p netip.Prefix) error {
	if err := canonicalIPv4(p); err != nil {
		return err
	}
	if !vpc.IsValid() || !vpc.Addr().Is4() || vpc.Bits() > p.Bits() || p.Bits() > 28 || !vpc.Contains(p.Addr()) {
		return invalid("subnet cidr must be inside the vpc and no longer than /28")
	}
	return nil
}

// Assignable reports whether a is a usable host in subnet. The network address,
// the next three addresses, and the broadcast address are reserved.
func Assignable(subnet netip.Prefix, a netip.Addr) bool {
	if !subnet.IsValid() || !subnet.Addr().Is4() || !a.Is4() || !subnet.Contains(a) {
		return false
	}
	network := ipv4ToUint(subnet.Masked().Addr())
	last := ipv4ToUint(prefixLast(subnet))
	u := ipv4ToUint(a)
	if u == network || u == last || (u >= network+1 && u <= network+3) {
		return false
	}
	return true
}

// NextAddress returns the lowest address in pool that is not taken and not
// inside avoid. The pool's network and broadcast addresses are never returned.
func NextAddress(pool netip.Prefix, taken func(netip.Addr) bool, avoid []netip.Prefix) (netip.Addr, bool) {
	if taken == nil || !pool.IsValid() || !pool.Addr().Is4() || pool.Bits() < 0 || pool.Bits() > 30 {
		return netip.Addr{}, false
	}
	start := ipv4ToUint(pool.Masked().Addr()) + 1
	last := ipv4ToUint(prefixLast(pool)) - 1
	if start > last {
		return netip.Addr{}, false
	}
	for n := start; ; n++ {
		a := uintToIPv4(n)
		if !taken(a) && !containsAny(a, avoid) {
			return a, true
		}
		if n == last {
			return netip.Addr{}, false
		}
	}
}

func canonicalIPv4(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || p != p.Masked() {
		return invalid("cidr must be a canonical ipv4 prefix")
	}
	return nil
}

func containsAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func prefixLast(p netip.Prefix) netip.Addr {
	hostBits := 32 - p.Bits()
	n := ipv4ToUint(p.Masked().Addr())
	if hostBits >= 32 {
		return uintToIPv4(0xffffffff)
	}
	n |= (uint32(1) << hostBits) - 1
	return uintToIPv4(n)
}

func ipv4ToUint(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uintToIPv4(n uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}
