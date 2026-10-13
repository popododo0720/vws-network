package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// DirectNetwork is one provider network that VM ports join directly, with no
// overlay (vws-deploy#125 "direct"). It shares the ProviderNetwork of any
// other underlay on the same NIC. The deploy side renders the list as the
// -kube-direct-networks JSON line; the field names below are that contract.
type DirectNetwork struct {
	Name            string       `json:"name"`
	ProviderNetwork string       `json:"providerNetwork"`
	Interface       string       `json:"interface"`
	VLANID          int          `json:"vlan"`
	CIDR            netip.Prefix `json:"cidr"`
	Gateway         netip.Addr   `json:"gateway"`
	Exclude         []string     `json:"exclude,omitempty"`
}

// DirectVPC holds every direct subnet in the API. It is not a stored VPC.
const DirectVPC domain.VPCID = "vpc-direct"

var directNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)

// directSubnetID is the API id of a direct network, shared by every project.
func directSubnetID(name string) domain.SubnetID { return domain.SubnetID("subnet-direct-" + name) }

// directKubeName names the Kube-OVN Subnet and the NAD of a direct network.
func directKubeName(name string) string { return "vws-direct-" + name }

// underlay returns the network as an UnderlayConfig so the external network's
// checks and object shapes apply unchanged.
func (d DirectNetwork) underlay() (UnderlayConfig, error) {
	u := UnderlayConfig{ProviderNetwork: d.ProviderNetwork, Interface: d.Interface, VLANID: d.VLANID, CIDR: d.CIDR, Gateway: d.Gateway}
	for _, e := range d.Exclude {
		r, err := ParseIPRange(e)
		if err != nil {
			return u, invalid("direct network %s: %v", d.Name, err)
		}
		u.Exclude = append(u.Exclude, r)
	}
	if err := u.validate(); err != nil {
		return u, invalid("direct network %s: %v", d.Name, err)
	}
	return u, nil
}

// validateDirect checks the direct networks against each other, the external
// underlay and the pool (vws-deploy#125 rules 4 and 7): names are unique, a
// provider network keeps one NIC, an (NIC, VLAN) pair carries one network, a
// NIC has at most one untagged underlay, and no CIDR overlaps another network
// or the pool.
func validateDirect(direct []DirectNetwork, ext UnderlayConfig, pool netip.Prefix) error {
	type l2 struct {
		nic  string
		vlan int
	}
	nicOf := map[string]string{}
	used := map[l2]string{}
	var cidrs []struct {
		name string
		p    netip.Prefix
	}
	add := func(name string, u UnderlayConfig) error {
		if nic, ok := nicOf[u.ProviderNetwork]; ok && nic != u.Interface {
			return invalid("provider network %s is on %s and %s; one provider network is one NIC", u.ProviderNetwork, nic, u.Interface)
		}
		for pn, nic := range nicOf {
			if nic == u.Interface && pn != u.ProviderNetwork {
				return invalid("%s uses provider network %s on %s, but %s already holds that NIC; Kube-OVN allows one provider network per NIC", name, u.ProviderNetwork, u.Interface, pn)
			}
		}
		nicOf[u.ProviderNetwork] = u.Interface
		if other, ok := used[l2{u.Interface, u.VLANID}]; ok {
			return invalid("%s and %s are both on %s vlan %d", name, other, u.Interface, u.VLANID)
		}
		used[l2{u.Interface, u.VLANID}] = name
		for _, c := range cidrs {
			if c.p.Overlaps(u.CIDR) {
				return invalid("%s cidr %v overlaps %s %v", name, u.CIDR, c.name, c.p)
			}
		}
		cidrs = append(cidrs, struct {
			name string
			p    netip.Prefix
		}{name, u.CIDR})
		return nil
	}
	if ext.enabled() {
		if err := add("the external network", ext); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, d := range direct {
		if !directNamePattern.MatchString(d.Name) {
			return invalid("direct network name %q must match %s", d.Name, directNamePattern)
		}
		if names[d.Name] {
			return invalid("direct network %s is listed twice", d.Name)
		}
		names[d.Name] = true
		u, err := d.underlay()
		if err != nil {
			return err
		}
		if pool.IsValid() && u.CIDR.Overlaps(pool) {
			return invalid("direct network %s cidr %v overlaps the elastic ip pool %v", d.Name, u.CIDR, pool)
		}
		if err := add("direct network "+d.Name, u); err != nil {
			return err
		}
	}
	return nil
}

// directSubnets is the API view of the direct networks, sorted by id.
func directSubnets(direct []DirectNetwork) []domain.Subnet {
	out := make([]domain.Subnet, 0, len(direct))
	for _, d := range direct {
		out = append(out, domain.Subnet{
			ID: directSubnetID(d.Name), VPC: DirectVPC, CIDR: d.CIDR,
			Labels: map[string]string{"vws:network-type": "direct"},
			Status: domain.StatusAvailable,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ensureDirect creates every direct network at start, after the external
// underlay: its ProviderNetwork (the same object when it shares the NIC with
// the external network), its Vlan, a Subnet whose provider ties it to a NAD of
// the same name, and that NAD in the VM namespace (vws-deploy#125), because a
// VM can only attach a NAD in its own namespace. As with the underlay, an object that differs
// is a Conflict and stops the start.
func (k *kubeOVN) ensureDirect(ctx context.Context) error {
	for _, d := range k.direct {
		u, err := d.underlay()
		if err != nil {
			return err
		}
		name := directKubeName(d.Name)
		objs := u.underlayObjects(name)
		objs[2].spec["provider"] = providerIn(k.directNADNamespace, domain.SubnetID(name))
		for _, o := range objs {
			if err := k.ensure(ctx, o.gvk, o.name, o.spec); err != nil {
				return fmt.Errorf("ensure direct network %s %s %s: %w", d.Name, o.gvk.Kind, o.name, err)
			}
		}
		if err := k.ensureNADIn(ctx, k.directNADNamespace, domain.SubnetID(name)); err != nil {
			return fmt.Errorf("ensure direct network %s nad: %w", d.Name, err)
		}
	}
	return nil
}

// validatePoolInExternalSubnet is the check for an external Subnet the
// operator made (no underlay flags): it reads the Subnet's cidrBlock and
// requires the elastic IP pool inside it, so every elastic IP is an address
// Kube-OVN can hand out on the external network (#52).
func (k *kubeOVN) validatePoolInExternalSubnet(ctx context.Context, pool netip.Prefix) error {
	if k.underlay.enabled() || k.externalSubnet == "" {
		return nil
	}
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkSubnet)
	if err := k.c.Get(ctx, client.ObjectKey{Name: k.externalSubnet}, o); err != nil {
		if apierrors.IsNotFound(err) {
			return invalid("external subnet %s does not exist", k.externalSubnet)
		}
		return fmt.Errorf("get external subnet %s: %w", k.externalSubnet, err)
	}
	block, _, err := unstructured.NestedString(o.Object, "spec", "cidrBlock")
	if err != nil {
		return invalid("external subnet %s cidrBlock: %v", k.externalSubnet, err)
	}
	cidr, err := netip.ParsePrefix(block)
	if err != nil {
		return invalid("external subnet %s cidrBlock %q is not an ipv4 prefix", k.externalSubnet, block)
	}
	cidr = cidr.Masked()
	if pool.Bits() < cidr.Bits() || !cidr.Contains(pool.Addr()) {
		return invalid("elastic ip pool %v is not inside external subnet %s cidr %v", pool, k.externalSubnet, cidr)
	}
	return nil
}

// isDirect reports a request aimed at the direct VPC or a direct subnet id.
// The names are reserved whether or not a direct network is configured.
func isDirect(vpc domain.VPCID, sub domain.SubnetID) bool {
	return vpc == DirectVPC || strings.HasPrefix(string(sub), "subnet-direct-")
}

func directReadOnly() error {
	return &domain.Error{Kind: domain.Forbidden, Msg: "UnauthorizedOperation: direct networks are read-only"}
}

func directElasticIP() error {
	return &domain.Error{Kind: domain.Invalid, Msg: "InvalidParameterCombination: an elastic ip cannot attach to a direct network interface"}
}

// onDirectOnly reports an association whose private address sits on a direct
// network and on no subnet of the elastic IP's VPC: that address is a direct
// ENI, which is already on the physical network and has no NAT to attach.
func (s *Service) onDirectOnly(st *domain.State, r domain.AssociateElasticIP) bool {
	e, ok := st.ElasticIPs[r.ElasticIP]
	if !ok {
		return false
	}
	for _, sub := range st.Subnets {
		if sub.VPC == e.VPC && sub.CIDR.Contains(r.Private) {
			return false
		}
	}
	for _, d := range s.direct {
		if d.CIDR.Contains(r.Private) {
			return true
		}
	}
	return false
}

// startChecks is the Kube-OVN work Open does before it serves: create the
// external underlay and the direct networks, and check the pool against the
// external Subnet. Each needs the API server, and serving without them would
// hand out elastic IPs Kube-OVN may not route or direct subnets that do not
// exist, so an API server it cannot reach stops the start. A domain error
// (Invalid, Conflict) is returned as is; anything else says why it stopped.
func (k *kubeOVN) startChecks(ctx context.Context, pool netip.Prefix) error {
	for _, step := range []func() error{
		func() error { return k.ensureUnderlay(ctx) },
		func() error { return k.ensureDirect(ctx) },
		func() error { return k.validatePoolInExternalSubnet(ctx, pool) },
	} {
		if err := step(); err != nil {
			var de *domain.Error
			if errors.As(err, &de) {
				return err
			}
			return fmt.Errorf("kube-ovn start checks need the API server, so vws-network does not start: it checks -pool against the external Subnet and creates the underlay and direct networks before it serves: %w", err)
		}
	}
	return nil
}
