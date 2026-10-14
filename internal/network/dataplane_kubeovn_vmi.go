package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	ip := k.vmiInterfaceIP(ctx, vmi, device)
	if ip == "" {
		return netip.Addr{}, fmt.Errorf("vmi %s/%s device %s: %w", k.vmNamespace, name, device, errInterfaceNotReady)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("vmi %s device %s reports address %q", name, device, ip)}
	}
	return addr, nil
}

// ifaceMatch is how pickVMIInterface chose an interface for a device.
type ifaceMatch int

const (
	ifaceNone    ifaceMatch = iota // nothing chosen
	ifaceExact                     // interfaceName or name equals the device
	ifaceSingle                    // no name matched; the VMI has one interface
	ifacePrimary                   // no name matched; nic0 is the pod network interface
)

// primaryDevice is the device name vws-console-api gives an instance's first
// and only wired NIC (internal/backend/grpcclient, DeviceName "nic0").
// vws-compute wires only that NIC, to the pod network (reconciler.go, "The
// first interface joins its subnet"; vws-compute#74).
const primaryDevice = "nic0"

// pickVMIInterface picks the status.interfaces[] entry of vmi for device and
// returns its ipAddress ("" while it has none), how it was picked, and the
// names KubeVirt reports, for the log.
//
//   - An entry whose interfaceName (the guest name, from the guest agent) or
//     name (the KubeVirt network name) equals device, interfaceName first.
//   - Otherwise, with exactly one entry, that entry. vws-compute sets no
//     interfaces, so KubeVirt adds one pod network named "default" (vws-test
//     77h: name "default", no interfaceName, podInterfaceName "eth0").
//   - Otherwise, for nic0 only, the pod network entry: the name of the
//     spec.networks[] entry with "pod", or "default" when spec.networks is
//     empty. A different device on a multi-NIC VMI is not guessed.
func pickVMIInterface(vmi *unstructured.Unstructured, device string) (string, ifaceMatch, []string) {
	raw, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces") //nolint:errcheck // absent and mistyped both read as empty
	var ifaces []map[string]any
	var names []string
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			ifaces = append(ifaces, m)
			names = append(names, nestedString(m, "name"))
		}
	}
	for _, field := range []string{"interfaceName", "name"} {
		for _, m := range ifaces {
			if nestedString(m, field) == device {
				return nestedString(m, "ipAddress"), ifaceExact, names
			}
		}
	}
	if len(ifaces) == 1 {
		return nestedString(ifaces[0], "ipAddress"), ifaceSingle, names
	}
	if device == primaryDevice && len(ifaces) > 1 {
		pod := podNetworkName(vmi)
		for _, m := range ifaces {
			if nestedString(m, "name") == pod {
				return nestedString(m, "ipAddress"), ifacePrimary, names
			}
		}
	}
	return "", ifaceNone, names
}

// podNetworkName is the name of the VMI's pod network: the spec.networks[]
// entry with a "pod" key, or KubeVirt's default "default" when there is none.
func podNetworkName(vmi *unstructured.Unstructured) string {
	nets, _, _ := unstructured.NestedSlice(vmi.Object, "spec", "networks") //nolint:errcheck // absent and mistyped both read as empty
	for _, n := range nets {
		if m, ok := n.(map[string]any); ok {
			if _, pod := m["pod"]; pod {
				return nestedString(m, "name")
			}
		}
	}
	return "default"
}

// The interface-pick warnings. using says which fallback was taken.
const (
	ifaceFallbackWarning = "network: vmi interface name does not match the device; using a fallback interface"
	ifaceMissingWarning  = "network: no vmi interface for the device"
)

// vmiInterfaceIP is pickVMIInterface for the first lookup and the VMI watch
// alike. A fallback pick or no pick at all is logged at WARN with the device
// and the interface names, once per VMI and device until the outcome changes,
// so the retries and watch events do not repeat it.
func (k *kubeOVN) vmiInterfaceIP(ctx context.Context, vmi *unstructured.Unstructured, device string) string {
	ip, how, names := pickVMIInterface(vmi, device)
	key := vmi.GetNamespace() + "/" + vmi.GetName() + "/" + device
	var msg, pick string
	switch {
	case how == ifaceSingle:
		msg, pick = ifaceFallbackWarning, "only"
	case how == ifacePrimary:
		msg, pick = ifaceFallbackWarning, "pod network"
	case how == ifaceNone && len(names) > 0:
		msg = ifaceMissingWarning
	}
	state := msg + "|" + pick + "|" + strings.Join(names, ",")
	k.warnMu.Lock()
	if k.warned == nil {
		k.warned = map[string]string{}
	}
	changed := k.warned[key] != state
	k.warned[key] = state
	k.warnMu.Unlock()
	if !changed {
		return ip
	}
	attrs := []slog.Attr{slog.String("vmi", vmi.GetName()), slog.String("device", device), slog.Any("interfaces", names)}
	switch msg {
	case ifaceFallbackWarning:
		slog.Default().LogAttrs(ctx, slog.LevelWarn, ifaceFallbackWarning, append(attrs, slog.String("using", pick))...)
	case ifaceMissingWarning:
		slog.Default().LogAttrs(ctx, slog.LevelWarn, ifaceMissingWarning, attrs...)
	}
	return ip
}

// rawWatchFrom starts a watch at resourceVersion rv, the list it follows.
func rawWatchFrom(rv string) *metav1.ListOptions {
	return &metav1.ListOptions{ResourceVersion: rv}
}
