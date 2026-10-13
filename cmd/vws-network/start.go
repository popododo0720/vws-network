package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/popododo0720/vws-network/internal/network"
)

const usage = "usage: vws-network -db PATH -pool CIDR [-listen ADDR] [-kubeconfig PATH] [-kube-external-subnet NAME [-kube-nad-namespace NS] [-kube-snat] [-kube-provider-network NAME -kube-underlay-interface NIC [-kube-underlay-vlan ID] -kube-underlay-cidr CIDR -kube-underlay-gateway IP [-kube-underlay-exclude A[..B],...]] [-kube-direct-networks JSON -kube-direct-nad-namespace NS]]"

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

// parseUnderlay turns the underlay flags into a config. network.Open checks
// the values; this only parses the addresses.
func parseUnderlay(provider, iface string, vlan int, cidr, gw, exclude string) (network.UnderlayConfig, error) {
	u := network.UnderlayConfig{ProviderNetwork: provider, Interface: iface, VLANID: vlan}
	var err error
	for _, e := range strings.Split(exclude, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		r, err := network.ParseIPRange(e)
		if err != nil {
			return u, fmt.Errorf("-kube-underlay-exclude: %w", err)
		}
		u.Exclude = append(u.Exclude, r)
	}
	if cidr != "" {
		if u.CIDR, err = netip.ParsePrefix(cidr); err != nil {
			return u, fmt.Errorf("-kube-underlay-cidr %q: %w", cidr, err)
		}
	}
	if gw != "" {
		if u.Gateway, err = netip.ParseAddr(gw); err != nil {
			return u, fmt.Errorf("-kube-underlay-gateway %q: %w", gw, err)
		}
	}
	return u, nil
}

// parseDirect reads -kube-direct-networks. Unknown fields are refused so a
// misspelt key fails the start instead of dropping a setting; Open checks
// the values.
func parseDirect(line string) ([]network.DirectNetwork, error) {
	if strings.TrimSpace(line) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	var out []network.DirectNetwork
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("-kube-direct-networks: %w", err)
	}
	if dec.More() {
		return nil, errors.New("-kube-direct-networks: trailing data after the json array")
	}
	return out, nil
}

// splitList reads a comma-separated flag, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}
