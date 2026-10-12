package network

import (
	"context"
	"fmt"
	"net/netip"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// subnet that asks, and later subnets reuse it, so its host fields are not compared.
// The OvnEip takes its address from the external subnet.
func (k *kubeOVN) ensureSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.ensureGateway(ctx, s); err != nil {
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

// ensureGateway makes the VPC gateway on host unless one already exists.
func (k *kubeOVN) ensureGateway(ctx context.Context, host domain.Subnet) error {
	return k.ensure(ctx, gvkNatGW, natGatewayName(host.VPC), map[string]any{
		"vpc":             string(host.VPC),
		"subnet":          string(host.ID),
		"lanIp":           natLanIP(host.CIDR).String(),
		"externalSubnets": []any{k.externalSubnet},
	}, "subnet", "lanIp")
}

// deleteSNAT removes the rule, then its address, then moves the gateway off
// this subnet.
func (k *kubeOVN) deleteSNAT(ctx context.Context, s domain.Subnet) error {
	if err := k.remove(ctx, gvkSnat, snatName(s.ID)); err != nil {
		return err
	}
	if err := k.remove(ctx, gvkEip, snatName(s.ID)); err != nil {
		return err
	}
	return k.rehomeGateway(ctx, s)
}

// rehomeGateway keeps the VPC gateway alive while any other subnet of the VPC
// remains. The gateway pod lives in its host subnet, so deleting the host
// deletes the gateway and creates it again on the first remaining subnet by
// name. With no subnet left the gateway goes. The state is only the cluster
// objects: after a crash between delete and create, the next call finds no
// gateway and a remaining subnet and creates it. A gateway that is still
// terminating makes the create fail with a retryable error. A gateway without
// our id annotation is not ours: it is a Conflict and nothing is deleted. The
// delete carries the UID read here, so a gateway replaced since is kept.
func (k *kubeOVN) rehomeGateway(ctx context.Context, gone domain.Subnet) error {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(gvkNatGW)
	err := k.c.Get(ctx, client.ObjectKey{Name: natGatewayName(gone.VPC)}, gw)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		if nestedString(gw.Object, "spec", "subnet") != string(gone.ID) {
			return nil
		}
		if err := k.removeObject(ctx, gw, natGatewayName(gone.VPC)); err != nil {
			return err
		}
	}
	next, ok, err := k.firstOtherSubnet(ctx, gone)
	if err != nil || !ok {
		return err
	}
	return k.ensureGateway(ctx, next)
}

// firstOtherSubnet returns the subnet of gone's VPC with the smallest name
// other than gone, skipping subnets that are being deleted.
func (k *kubeOVN) firstOtherSubnet(ctx context.Context, gone domain.Subnet) (domain.Subnet, bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvkSubnet.GroupVersion().WithKind("SubnetList"))
	if err := k.c.List(ctx, list); err != nil {
		return domain.Subnet{}, false, err
	}
	var best *unstructured.Unstructured
	for i := range list.Items {
		o := &list.Items[i]
		if nestedString(o.Object, "spec", "vpc") != string(gone.VPC) {
			continue
		}
		if o.GetName() == string(gone.ID) || o.GetDeletionTimestamp() != nil {
			continue
		}
		if best == nil || o.GetName() < best.GetName() {
			best = o
		}
	}
	if best == nil {
		return domain.Subnet{}, false, nil
	}
	cidr := nestedString(best.Object, "spec", "cidrBlock")
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return domain.Subnet{}, false, fmt.Errorf("subnet %s has cidrBlock %q: %w", best.GetName(), cidr, err)
	}
	return domain.Subnet{ID: domain.SubnetID(best.GetName()), VPC: gone.VPC, CIDR: p}, true, nil
}
