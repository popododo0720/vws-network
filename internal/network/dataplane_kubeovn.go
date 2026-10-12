package network

import (
	"context"
	"net/netip"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// annotationID holds the AWS-style id on every object this dataplane creates.
// The object name is the same id, so a lookup never lists.
const annotationID = "vws.popododo0720.io/id"

var (
	gvkVpc    = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Vpc"}
	gvkSubnet = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Subnet"}
	gvkGroup  = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "SecurityGroup"}

	anyIPv4 = netip.MustParsePrefix("0.0.0.0/0")
)

// kubeOVN is the dataplane backed by Kube-OVN custom resources. Every object
// is cluster scoped and named by its resource id. Create never overwrites:
// CIDRs are immutable, so an existing object already is the desired state.
type kubeOVN struct {
	c client.Client
}

func (k *kubeOVN) ensure(ctx context.Context, gvk schema.GroupVersionKind, id string, spec map[string]any) error {
	o := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	o.SetGroupVersionKind(gvk)
	o.SetName(id)
	o.SetAnnotations(map[string]string{annotationID: id})
	return client.IgnoreAlreadyExists(k.c.Create(ctx, o))
}

func (k *kubeOVN) remove(ctx context.Context, gvk schema.GroupVersionKind, id string) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetName(id)
	return client.IgnoreNotFound(k.c.Delete(ctx, o))
}

func (k *kubeOVN) ensureVPC(ctx context.Context, v domain.VPC) error {
	if err := domain.CheckVPCCIDR(v.CIDR); err != nil {
		return err
	}
	return k.ensure(ctx, gvkVpc, string(v.ID), map[string]any{})
}

func (k *kubeOVN) deleteVPC(ctx context.Context, v domain.VPC) error {
	return k.remove(ctx, gvkVpc, string(v.ID))
}

func (k *kubeOVN) ensureSubnet(ctx context.Context, s domain.Subnet) error {
	if err := domain.CheckSubnetCIDR(anyIPv4, s.CIDR); err != nil {
		return err
	}
	return k.ensure(ctx, gvkSubnet, string(s.ID), map[string]any{
		"vpc":       string(s.VPC),
		"cidrBlock": s.CIDR.String(),
		"protocol":  "IPv4",
	})
}

func (k *kubeOVN) deleteSubnet(ctx context.Context, s domain.Subnet) error {
	return k.remove(ctx, gvkSubnet, string(s.ID))
}

func (k *kubeOVN) ensureElasticIP(context.Context, domain.ElasticIP) error { return errNotImplemented }
func (k *kubeOVN) deleteElasticIP(context.Context, domain.ElasticIP) error { return errNotImplemented }
func (k *kubeOVN) ensureNAT(context.Context, domain.ElasticIP, netip.Addr) error {
	return errNotImplemented
}
func (k *kubeOVN) deleteNAT(context.Context, domain.ElasticIP, netip.Addr) error {
	return errNotImplemented
}

var errNotImplemented = &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn dataplane: not implemented"}

var _ dataplane = (*kubeOVN)(nil)
