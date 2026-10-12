package network

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	gvkEip    = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "OvnEip"}
	gvkFip    = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "OvnFip"}
	gvkNAD    = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}

	anyIPv4 = netip.MustParsePrefix("0.0.0.0/0")
)

// kubeOVN is the dataplane backed by Kube-OVN custom resources. Every object
// is cluster scoped and named by its resource id. Create never overwrites:
// an existing object must carry the same id and spec, or ensure fails.
//
// externalSubnet names the Kube-OVN Subnet that holds the elastic IP pool.
// It is the only place the pool is named; addresses come from the domain.
type kubeOVN struct {
	c              client.Client
	externalSubnet string
	// nadNamespace holds one NetworkAttachmentDefinition per subnet.
	nadNamespace string
	// snat gives every subnet outbound SNAT through externalSubnet.
	snat bool
}

// ensure creates the object, or accepts the existing one when it carries the
// same id annotation and every field of spec we set is equal. A different id
// or a different field is a Conflict: it never retries and never overwrites,
// because an overwrite would break a running resource. Fields named in ignore
// belong to another writer and are not compared.
func (k *kubeOVN) ensure(ctx context.Context, gvk schema.GroupVersionKind, id string, spec map[string]any, ignore ...string) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetName(id)
	return k.ensureObject(ctx, o, id, spec, ignore...)
}

func (k *kubeOVN) ensureObject(ctx context.Context, o *unstructured.Unstructured, id string, spec map[string]any, ignore ...string) error {
	key := client.ObjectKeyFromObject(o)
	want := o.DeepCopy()
	want.Object["spec"] = spec
	want.SetAnnotations(map[string]string{annotationID: id})
	err := k.c.Create(ctx, want)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	have := o.DeepCopy()
	if err := k.c.Get(ctx, key, have); err != nil {
		return err
	}
	kind := o.GetKind()
	if have.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%s %s is being deleted", kind, key)
	}
	if got := have.GetAnnotations()[annotationID]; got != id {
		return &domain.Error{Kind: domain.Conflict, Msg: fmt.Sprintf("%s %s has id annotation %q, want %q", kind, key, got, id)}
	}
	haveSpec, _, _ := unstructured.NestedMap(have.Object, "spec")
	for field, w := range spec {
		if slices.Contains(ignore, field) {
			continue
		}
		if !reflect.DeepEqual(haveSpec[field], w) {
			return &domain.Error{Kind: domain.Conflict, Msg: fmt.Sprintf("%s %s has spec.%s %v, want %v", kind, key, field, haveSpec[field], w)}
		}
	}
	return nil
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
	if k.snat && k.externalSubnet == "" {
		return &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn external subnet is not configured"}
	}
	if err := k.ensureNAD(ctx, s.ID); err != nil {
		return err
	}
	if err := k.ensure(ctx, gvkSubnet, string(s.ID), map[string]any{
		"vpc":       string(s.VPC),
		"cidrBlock": s.CIDR.String(),
		"protocol":  "IPv4",
		"provider":  k.nadProvider(s.ID),
	}); err != nil {
		return err
	}
	if k.snat {
		return k.ensureSNAT(ctx, s)
	}
	return nil
}

func (k *kubeOVN) deleteSubnet(ctx context.Context, s domain.Subnet) error {
	if k.snat {
		if err := k.deleteSNAT(ctx, s); err != nil {
			return err
		}
	}
	if err := k.remove(ctx, gvkSubnet, string(s.ID)); err != nil {
		return err
	}
	return k.removeNAD(ctx, s.ID)
}

var _ dataplane = (*kubeOVN)(nil)
