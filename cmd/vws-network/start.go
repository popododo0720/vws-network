package main

import (
	"errors"
	"fmt"
	"net/netip"
)

const usage = "usage: vws-network -db PATH -pool CIDR [-listen ADDR] [-kubeconfig PATH] [-kube-external-subnet NAME [-kube-nad-namespace NS] [-kube-snat]]"

// parseStart checks the required flags before anything opens. -pool has no
// default: the site's elastic IP range must be given explicitly.
func parseStart(db, pool string) (netip.Prefix, error) {
	if db == "" {
		return netip.Prefix{}, errors.New("-db is required")
	}
	if pool == "" {
		return netip.Prefix{}, errors.New("-pool is required: set the site's elastic IP CIDR (there is no default)")
	}
	p, err := netip.ParsePrefix(pool)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("-pool %q: %w", pool, err)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("-pool %s is not canonical: use %s", p, p.Masked())
	}
	for _, d := range documentationRanges {
		if d.Overlaps(p) {
			return netip.Prefix{}, fmt.Errorf("-pool %s overlaps the documentation range %s: set the site's real elastic IP CIDR", p, d)
		}
	}
	return p, nil
}

// documentationRanges are the RFC 5737 TEST-NET blocks. They are
// placeholders (the old chart default was 192.0.2.0/24), never a site's
// elastic IP range, so the binary refuses them. network.Open still checks
// the shape of the prefix.
var documentationRanges = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}
