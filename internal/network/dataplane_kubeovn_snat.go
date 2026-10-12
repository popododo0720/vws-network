package network

import (
	"context"
	"net/netip"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

var (
	gvkSnat  = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "OvnSnatRule"}
	gvkNatGW = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "VpcNatGateway"}
)

// natGatewayName is the one place a VpcNatGateway name is made. A VPC has one.
func natGatewayName(v domain.VPCID) string { return "nat-" + string(v) }

// snatName names the OvnEip and the OvnSnatRule of one subnet.
func snatName(s domain.SubnetID) string { return "snat-" + string(s) }

// natLanIP is the last usable host of the subnet, which Kube-OVN leaves free
// of the gateway at the first host.
func natLanIP(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	size := uint32(1) << (32 - p.Bits())
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n += size - 2
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// ensureSNAT gives a subnet outbound SNAT. The VPC gateway lives on the first
// subnet that asks, and later subnets reuse it. The OvnEip takes its address
// from the external subnet.
func (k *kubeOVN) ensureSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.ensure(ctx, gvkNatGW, natGatewayName(s.VPC), map[string]any{
		"vpc":             string(s.VPC),
		"subnet":          string(s.ID),
		"lanIp":           natLanIP(s.CIDR).String(),
		"externalSubnets": []any{k.externalSubnet},
	}); err != nil {
		return err
	}
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

// deleteSNAT removes the rule, then its address, then the gateway when this
// subnet hosts it.
func (k *kubeOVN) deleteSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.remove(ctx, gvkSnat, snatName(s.ID)); err != nil {
		return err
	}
	if err := k.remove(ctx, gvkEip, snatName(s.ID)); err != nil {
		return err
	}
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(gvkNatGW)
	err := k.c.Get(ctx, client.ObjectKey{Name: natGatewayName(s.VPC)}, gw)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if host, _, _ := unstructured.NestedString(gw.Object, "spec", "subnet"); host != string(s.ID) {
		return nil
	}
	return client.IgnoreNotFound(k.c.Delete(ctx, gw))
}
