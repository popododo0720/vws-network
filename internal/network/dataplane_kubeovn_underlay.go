package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

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
	// Exclude are addresses on the physical network that Kube-OVN must never
	// hand out (routers, nodes, other hosts). The gateway is always excluded.
	Exclude []IPRange
}

// IPRange is an inclusive IPv4 range; From == To is one address. It renders
// in the Kube-OVN excludeIps form: "a" or "a..b".
type IPRange struct{ From, To netip.Addr }

func (r IPRange) String() string {
	if r.From == r.To {
		return r.From.String()
	}
	return r.From.String() + ".." + r.To.String()
}

// ParseIPRange reads "a" or "a..b".
func ParseIPRange(s string) (IPRange, error) {
	from, to, ok := strings.Cut(s, "..")
	if !ok {
		to = from
	}
	a, err := netip.ParseAddr(from)
	if err != nil {
		return IPRange{}, fmt.Errorf("exclude %q: %w", s, err)
	}
	b, err := netip.ParseAddr(to)
	if err != nil {
		return IPRange{}, fmt.Errorf("exclude %q: %w", s, err)
	}
	return IPRange{From: a, To: b}, nil
}

func (r IPRange) overlaps(p netip.Prefix) bool {
	last := lastAddr(p)
	return !(r.To.Less(p.Addr()) || last.Less(r.From))
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= ^uint32(0) >> p.Bits()
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// excludeIPs is the Subnet excludeIps: the gateway first, then the operator's
// ranges in the order given, without repeating the gateway.
func (u UnderlayConfig) excludeIPs() []any {
	out := []any{u.Gateway.String()}
	for _, r := range u.Exclude {
		if r.String() != u.Gateway.String() {
			out = append(out, r.String())
		}
	}
	return out
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
	for _, r := range u.Exclude {
		if !r.From.Is4() || !r.To.Is4() || r.To.Less(r.From) || !u.CIDR.Contains(r.From) || !u.CIDR.Contains(r.To) {
			return invalid("kube-ovn underlay exclude %v must be an ascending ipv4 range inside %v", r, u.CIDR)
		}
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
	for _, r := range u.Exclude {
		if r.overlaps(pool) {
			return invalid("elastic ip pool %v overlaps the underlay exclude %v", pool, r)
		}
	}
	return nil
}

var (
	gvkProviderNetwork = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "ProviderNetwork"}
	gvkVlan            = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Vlan"}
)

// underlayObjects is the external network in creation order: the
// ProviderNetwork names the node NIC, the Vlan tags it (0 is flat) and the
// Subnet carries the elastic IP CIDR on that Vlan. Each object is named by
// itself and carries that name as its id, so ensure never adopts an object
// someone else made with the same name.
func (u UnderlayConfig) underlayObjects(externalSubnet string) []underlayObject {
	return []underlayObject{
		{gvkProviderNetwork, u.ProviderNetwork, map[string]any{"defaultInterface": u.Interface}},
		{gvkVlan, u.vlanName(), map[string]any{"id": int64(u.VLANID), "provider": u.ProviderNetwork}},
		{gvkSubnet, externalSubnet, map[string]any{
			"protocol":   "IPv4",
			"cidrBlock":  u.CIDR.String(),
			"gateway":    u.Gateway.String(),
			"vlan":       u.vlanName(),
			"excludeIps": u.excludeIPs(),
		}},
	}
}

type underlayObject struct {
	gvk  schema.GroupVersionKind
	name string
	spec map[string]any
}

// ensureUnderlay creates the external network at start. An object that
// exists with another id or spec is a Conflict and stops the start: the
// service never rewires a live physical network.
func (k *kubeOVN) ensureUnderlay(ctx context.Context) error {
	if !k.underlay.enabled() {
		return nil
	}
	for _, o := range k.underlay.underlayObjects(k.externalSubnet) {
		if err := k.ensure(ctx, o.gvk, o.name, o.spec); err != nil {
			return fmt.Errorf("ensure underlay %s %s: %w", o.gvk.Kind, o.name, err)
		}
	}
	return nil
}
