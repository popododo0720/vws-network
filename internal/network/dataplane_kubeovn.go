package network

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// annotationPrefix starts every annotation key a VWS service writes, in every
// repository. A key is annotationPrefix plus a short lowercase name.
const annotationPrefix = "vws.popododo0720.io/"

// annotationID holds the AWS-style id on every object this dataplane creates.
// The object name is the same id, so a lookup never lists.
const annotationID = annotationPrefix + "id"

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
	// underlay, when enabled, is the external network this service creates.
	underlay UnderlayConfig
	// direct are the direct provider networks this service creates.
	direct []DirectNetwork
	// directNADNamespace is the VM namespace that holds the direct NADs.
	directNADNamespace string
	// vmNamespace is where vws-compute runs instances as VMIs.
	vmNamespace string
	// w watches VMIs. It is the same client as c; tests that build a
	// kubeOVN by hand may leave it nil, and then nothing is watched.
	w client.WithWatch
	// vmMTU is spec.mtu of every VPC subnet; Kube-OVN puts it on the VM
	// interfaces and in the subnet's DHCPv4 options. 0 (a hand-built test
	// kubeOVN) leaves Kube-OVN's own default.
	vmMTU int
	// gatewayNodes are the only nodes this service labels as external gateways.
	gatewayNodes []string
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
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensureObject: %w", err)
	}
	have := o.DeepCopy()
	if err := k.c.Get(ctx, key, have); err != nil {
		return fmt.Errorf("ensureObject: %w", err)
	}
	kind := o.GetKind()
	if have.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%s %s is being deleted", kind, key)
	}
	if got := have.GetAnnotations()[annotationID]; got != id {
		return &domain.Error{Kind: domain.Conflict, Msg: fmt.Sprintf("%s %s has id annotation %q, want %q", kind, key, got, id)}
	}
	haveSpec := nestedMap(have.Object, "spec")
	// Sorted, so a Conflict always names the same first differing field.
	for _, field := range slices.Sorted(maps.Keys(spec)) {
		w := spec[field]
		if slices.Contains(ignore, field) {
			continue
		}
		if !reflect.DeepEqual(haveSpec[field], w) {
			return &domain.Error{Kind: domain.Conflict, Msg: fmt.Sprintf("%s %s has spec.%s %v, want %v", kind, key, field, haveSpec[field], w)}
		}
	}
	return nil
}

// nestedString reads a string field of an object that came from the cluster.
// A missing or mistyped field reads as empty, which never equals a resource id.
func nestedString(obj map[string]any, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj, fields...) //nolint:errcheck // absent and mistyped both read as empty
	return v
}

// nestedMap is nestedString for a map field.
func nestedMap(obj map[string]any, fields ...string) map[string]any {
	v, _, _ := unstructured.NestedMap(obj, fields...) //nolint:errcheck // absent and mistyped both read as empty
	return v
}

// ignoreNotFound treats a missing object as success and wraps any other error.
func ignoreNotFound(op string, err error) error {
	if err = client.IgnoreNotFound(err); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// remove deletes the object only when it carries the id annotation for id. A
// missing object is success. An object without the annotation, or with another
// id, was not created by this dataplane and is a Conflict that deletes nothing.
func (k *kubeOVN) remove(ctx context.Context, gvk schema.GroupVersionKind, id string) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetName(id)
	return k.removeObject(ctx, o, id)
}

// removeObject reads o by its key and deletes it under a UID precondition, so
// an object replaced after the read is not deleted.
func (k *kubeOVN) removeObject(ctx context.Context, o *unstructured.Unstructured, id string) error {
	key := client.ObjectKeyFromObject(o)
	have := o.DeepCopy()
	if err := k.c.Get(ctx, key, have); err != nil {
		return ignoreNotFound("remove", err)
	}
	if got := have.GetAnnotations()[annotationID]; got != id {
		return &domain.Error{Kind: domain.Conflict, Msg: fmt.Sprintf("%s %s has id annotation %q, want %q: not deleting", o.GetKind(), key, got, id)}
	}
	uid := have.GetUID()
	return ignoreNotFound("remove", k.c.Delete(ctx, have, client.Preconditions{UID: &uid}))
}

func (k *kubeOVN) ensureVPC(ctx context.Context, v domain.VPC) error {
	if err := domain.CheckVPCCIDR(v.CIDR); err != nil {
		return err
	}
	gw, err := k.externalGateway(ctx)
	if err != nil {
		return err
	}
	// enableExternal and extraExternalSubnets attach the VPC router to the
	// external Subnet, which OvnEip, OvnFip and OvnSnatRule need. The default
	// route sends traffic that leaves the VPC to the external gateway; without
	// it SNAT matches nothing (vws-test 42, 43). The gateway ConfigMap
	// ovn-external-gw-config belongs to the deploy side (vws-deploy#127).
	return k.ensure(ctx, gvkVpc, string(v.ID), map[string]any{
		"enableExternal":       true,
		"extraExternalSubnets": []any{k.externalSubnet},
		"staticRoutes": []any{map[string]any{
			"cidr":      anyIPv4.String(),
			"nextHopIP": gw.String(),
			"policy":    "policyDst",
		}},
	})
}

// externalGateway is the next hop of every VPC default route: the underlay
// gateway when this service made the external Subnet, otherwise the gateway
// the operator gave that Subnet.
func (k *kubeOVN) externalGateway(ctx context.Context) (netip.Addr, error) {
	if k.underlay.enabled() {
		return k.underlay.Gateway, nil
	}
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkSubnet)
	if err := k.c.Get(ctx, client.ObjectKey{Name: k.externalSubnet}, o); err != nil {
		return netip.Addr{}, fmt.Errorf("get external subnet %s: %w", k.externalSubnet, err)
	}
	gw, err := netip.ParseAddr(nestedString(o.Object, "spec", "gateway"))
	if err != nil || !gw.Is4() {
		return netip.Addr{}, &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("external subnet %s has no ipv4 spec.gateway for the vpc default route", k.externalSubnet)}
	}
	return gw, nil
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
	if err := k.ensure(ctx, gvkSubnet, string(s.ID), k.subnetSpec(s)); err != nil {
		return err
	}
	if k.snat {
		return k.ensureSNAT(ctx, s)
	}
	return nil
}

// subnetSpec is a VPC subnet's Kube-OVN spec. With vmMTU set it carries
// spec.mtu, enableDHCP and dhcpV4Options "lease_time=3600,mtu=<mtu>".
// Kube-OVN v1.16.12 gives the pod interface spec.mtu, and builds the
// subnet's OVN DHCP_Options from dhcpV4Options (an options string, despite the
// CRD's "UUID" description), filling the missing router, server_id and
// server_mac itself (pkg/ovs/util.go buildDHCPv4Options). mtu is written out
// so the DHCP value is the flag's, not one Kube-OVN derives.
func (k *kubeOVN) subnetSpec(s domain.Subnet) map[string]any {
	spec := map[string]any{
		"vpc":       string(s.VPC),
		"cidrBlock": s.CIDR.String(),
		"protocol":  "IPv4",
		"provider":  k.nadProvider(s.ID),
	}
	if k.vmMTU > 0 {
		spec["mtu"] = int64(k.vmMTU)
		spec["enableDHCP"] = true
		spec["dhcpV4Options"] = fmt.Sprintf("lease_time=3600,mtu=%d", k.vmMTU)
	}
	return spec
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

// ready reads the external Subnet. It proves the API server answers, the
// Kube-OVN CRDs are installed and the service account can read them.
func (k *kubeOVN) ready(ctx context.Context) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkSubnet)
	if err := k.c.Get(ctx, client.ObjectKey{Name: k.externalSubnet}, o); err != nil {
		return fmt.Errorf("get external subnet %s: %w", k.externalSubnet, err)
	}
	if k.underlay.enabled() {
		pn := &unstructured.Unstructured{}
		pn.SetGroupVersionKind(gvkProviderNetwork)
		if err := k.c.Get(ctx, client.ObjectKey{Name: k.underlay.ProviderNetwork}, pn); err != nil {
			return fmt.Errorf("get provider network %s: %w", k.underlay.ProviderNetwork, err)
		}
	}
	return nil
}
