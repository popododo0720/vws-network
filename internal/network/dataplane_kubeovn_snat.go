package network

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/popododo0720/vws-network/internal/domain"
)

var gvkSnat = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "OvnSnatRule"}

// snatName names the OvnEip and the OvnSnatRule of one subnet.
func snatName(s domain.SubnetID) string { return "snat-" + string(s) }

// ensureSNAT gives a subnet outbound SNAT with OVN-native NAT only: an OvnEip
// of type nat from the external Subnet and an OvnSnatRule on the subnet. The
// VPC router does the NAT, so there is no VpcNatGateway (iptables) pod. That
// pod needs a NAD named after the external Subnet in kube-system and never
// started on vws-test (40, 41); OVN SNAT worked (42, 43).
func (k *kubeOVN) ensureSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.ensure(ctx, gvkEip, snatName(s.ID), map[string]any{
		"externalSubnet": k.externalSubnet,
		"type":           "nat",
	}); err != nil {
		return err
	}
	return k.ensure(ctx, gvkSnat, snatName(s.ID), map[string]any{
		"ovnEip":    snatName(s.ID),
		"vpcSubnet": string(s.ID),
	})
}

// deleteSNAT removes the rule, then its address. Kube-OVN refuses to drop an
// OvnEip a rule still uses, so the order matters (vws-test 45b).
func (k *kubeOVN) deleteSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.remove(ctx, gvkSnat, snatName(s.ID)); err != nil {
		return err
	}
	return k.remove(ctx, gvkEip, snatName(s.ID))
}
