package domain

import (
	"net/netip"
	"sort"
	"strconv"
)

// CheckRule checks the shape of one rule. A peer CIDR is any canonical IPv4
// prefix, including 0.0.0.0/0. It is not a VPC CIDR and is not passed to
// CheckVPCCIDR. TCP and UDP ports are 0 through 65535 with from <= to.
// ICMP and all use port -1. Exactly one of peer CIDR and peer group is set.
func CheckRule(r Rule) error {
	switch r.Direction {
	case Ingress, Egress:
	default:
		return invalid("rule direction is required")
	}
	switch r.Protocol {
	case TCP, UDP:
		if r.FromPort < 0 || r.ToPort > 65535 || r.FromPort > r.ToPort {
			return invalid("tcp and udp ports must be 0 through 65535")
		}
	case ICMP, All:
		if r.FromPort != -1 || r.ToPort != -1 {
			return invalid("icmp and all rules use port -1")
		}
	default:
		return invalid("rule protocol is required")
	}
	hasCIDR := r.Peer.CIDR.IsValid()
	hasGroup := r.Peer.Group != ""
	if hasCIDR == hasGroup {
		return invalid("rule peer must be a cidr or a security group")
	}
	if hasCIDR {
		if err := canonicalIPv4(r.Peer.CIDR); err != nil {
			return invalid("rule peer cidr must be a canonical ipv4 prefix")
		}
	}
	return nil
}

// CanonicalRules checks, sorts, and deduplicates rules so one set has one fingerprint.
// An empty list is invalid.
func CanonicalRules(rs []Rule) ([]Rule, error) {
	if len(rs) == 0 {
		return nil, invalid("rules are required")
	}
	out := make([]Rule, len(rs))
	copy(out, rs)
	for i := range out {
		if err := CheckRule(out[i]); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return ruleKey(out[i]) < ruleKey(out[j]) })
	deduped := out[:0]
	for i, r := range out {
		if i > 0 && r == out[i-1] {
			continue
		}
		deduped = append(deduped, r)
	}
	return deduped, nil
}

// Identity is the authorize and revoke tuple. Equal rules share an identity.
func (r Rule) Identity() string { return ruleKey(r) }

// DefaultEgress is the egress allow-all rule a new security group starts with.
func DefaultEgress() Rule {
	return Rule{
		Direction: Egress,
		Protocol:  All,
		FromPort:  -1,
		ToPort:    -1,
		Peer:      Peer{CIDR: netip.MustParsePrefix("0.0.0.0/0")},
	}
}

func ruleKey(r Rule) string {
	peer := r.Peer.CIDR.String()
	if r.Peer.Group != "" {
		peer = "sg:" + string(r.Peer.Group)
	}
	return strconv.Itoa(int(r.Direction)) + "|" + strconv.Itoa(int(r.Protocol)) + "|" +
		strconv.Itoa(int(r.FromPort)) + "|" + strconv.Itoa(int(r.ToPort)) + "|" + peer
}
