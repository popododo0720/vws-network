package network

import (
	"fmt"
	"net/netip"

	"github.com/popododo0720/vws-network/internal/domain"
)

// UnderlayConfig wires the external network straight to a node NIC through
// Kube-OVN underlay: one ProviderNetwork on Interface, one Vlan (ID 0 is a
// flat network, 1-4094 a tagged one) and the external Subnet named by
// KubeOVNConfig.ExternalSubnet with CIDR and Gateway. A zero UnderlayConfig
// leaves the external Subnet to the operator, as before.
//
// The node network (which NIC, which VLAN, which CIDR) is owned by the deploy
// side. This service only takes the values as flags; see docs/underlay.md.
type UnderlayConfig struct {
	ProviderNetwork string
	Interface       string
	VLANID          int
	CIDR            netip.Prefix
	Gateway         netip.Addr
}

func (u UnderlayConfig) enabled() bool {
	return u.ProviderNetwork != "" || u.Interface != "" || u.VLANID != 0 || u.CIDR.IsValid() || u.Gateway.IsValid()
}

// vlanName is the Vlan object the external Subnet points at. Kube-OVN keeps
// Vlan cluster scoped, so the name carries the provider network and the tag.
func (u UnderlayConfig) vlanName() string {
	return fmt.Sprintf("%s-vlan%d", u.ProviderNetwork, u.VLANID)
}

func invalid(format string, a ...any) error {
	return &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf(format, a...)}
}

// validate checks the underlay on its own. validatePoolInUnderlay checks it
// against the elastic IP pool once that is known.
func (u UnderlayConfig) validate() error {
	switch {
	case u.ProviderNetwork == "":
		return invalid("kube-ovn underlay needs a provider network name")
	case len(u.ProviderNetwork) > 12:
		// Kube-OVN derives OVS bridge and port names from it; it caps the name at 12.
		return invalid("kube-ovn provider network %q is longer than 12 characters", u.ProviderNetwork)
	case u.Interface == "":
		return invalid("kube-ovn underlay needs the node interface of provider network %s", u.ProviderNetwork)
	case u.VLANID < 0 || u.VLANID > 4094:
		return invalid("kube-ovn underlay vlan id %d is outside 0 (flat) through 4094", u.VLANID)
	case !u.CIDR.IsValid() || !u.CIDR.Addr().Is4() || u.CIDR != u.CIDR.Masked():
		return invalid("kube-ovn underlay cidr must be a canonical ipv4 prefix")
	case !u.Gateway.IsValid() || !u.CIDR.Contains(u.Gateway):
		return invalid("kube-ovn underlay gateway %v is not inside %v", u.Gateway, u.CIDR)
	}
	return nil
}

// validatePoolInUnderlay keeps every elastic IP routable on the physical
// network: the pool lies inside the external CIDR and never holds the gateway.
func validatePoolInUnderlay(u UnderlayConfig, pool netip.Prefix) error {
	if !u.enabled() {
		return nil
	}
	if err := u.validate(); err != nil {
		return err
	}
	if pool.Bits() < u.CIDR.Bits() || !u.CIDR.Contains(pool.Addr()) {
		return invalid("elastic ip pool %v is not inside the underlay cidr %v", pool, u.CIDR)
	}
	if pool.Contains(u.Gateway) {
		return invalid("elastic ip pool %v holds the underlay gateway %v", pool, u.Gateway)
	}
	return nil
}
