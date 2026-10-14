package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// gvkVMI is the KubeVirt VirtualMachineInstance vws-compute runs an instance
// as. It is named after the instance id in the VM namespace.
var gvkVMI = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}

// errInterfaceNotReady is an attachment whose instance interface has no
// address yet: the VMI is missing, or KubeVirt has not reported the
// interface or its IP. The worker retries it without giving up.
var errInterfaceNotReady = errors.New("instance interface has no address yet")

// interfaceAddress reads the IPv4 address KubeVirt reports for device on the
// instance's VMI. device matches status.interfaces[].interfaceName (the guest
// name, eth0) or, failing that, .name (the KubeVirt network name).
func (k *kubeOVN) interfaceAddress(ctx context.Context, instance, device string) (netip.Addr, error) {
	if k.vmNamespace == "" {
		return netip.Addr{}, &domain.Error{Kind: domain.Invalid, Msg: "an address attached to an instance needs the VM namespace (-kube-vm-namespace)"}
	}
	name := instance[strings.LastIndex(instance, "/")+1:]
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(gvkVMI)
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: k.vmNamespace, Name: name}, vmi); err != nil {
		if apierrors.IsNotFound(err) {
			return netip.Addr{}, fmt.Errorf("vmi %s/%s: %w", k.vmNamespace, name, errInterfaceNotReady)
		}
		return netip.Addr{}, fmt.Errorf("get vmi %s/%s: %w", k.vmNamespace, name, err)
	}
	ip := vmiInterfaceIP(vmi, device)
	if ip == "" {
		return netip.Addr{}, fmt.Errorf("vmi %s/%s device %s: %w", k.vmNamespace, name, device, errInterfaceNotReady)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("vmi %s device %s reports address %q", name, device, ip)}
	}
	return addr, nil
}

// vmiInterfaceIP is the ipAddress KubeVirt reports for device on vmi, matched
// on status.interfaces[].interfaceName (the guest name, eth0) first and then
// .name (the KubeVirt network name). It is "" while there is none.
func vmiInterfaceIP(vmi *unstructured.Unstructured, device string) string {
	ifaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces") //nolint:errcheck // absent and mistyped both read as empty
	for _, field := range []string{"interfaceName", "name"} {
		for _, raw := range ifaces {
			m, ok := raw.(map[string]any)
			if ok && nestedString(m, field) == device {
				if ip := nestedString(m, "ipAddress"); ip != "" {
					return ip
				}
			}
		}
	}
	return ""
}

// rawWatchFrom starts a watch at resourceVersion rv, the list it follows.
func rawWatchFrom(rv string) *metav1.ListOptions {
	return &metav1.ListOptions{ResourceVersion: rv}
}
