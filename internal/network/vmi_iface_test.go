package network

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func vmiObj(networks []any, ifaces ...map[string]any) *unstructured.Unstructured {
	st := make([]any, 0, len(ifaces))
	for _, i := range ifaces {
		st = append(st, i)
	}
	spec := map[string]any{}
	if networks != nil {
		spec["networks"] = networks
	}
	o := &unstructured.Unstructured{Object: map[string]any{"spec": spec, "status": map[string]any{"interfaces": st}}}
	o.SetNamespace("vms")
	o.SetName("i-1")
	return o
}

// captureWarn sends slog to a buffer for the test.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestPickVMIInterface(t *testing.T) {
	// vws-test 77h: one pod network "default", no guest agent interfaceName.
	only := map[string]any{"name": "default", "podInterfaceName": "eth0", "ipAddress": "172.31.76.3"}
	multus := map[string]any{"name": "net1", "interfaceName": "eth1", "ipAddress": "10.9.0.4"}
	for _, tc := range []struct {
		name   string
		vmi    *unstructured.Unstructured
		device string
		ip     string
		how    ifaceMatch
	}{
		{"exact interfaceName", vmiObj(nil, map[string]any{"name": "default", "interfaceName": "eth0", "ipAddress": "10.0.0.5"}), "eth0", "10.0.0.5", ifaceExact},
		{"exact name", vmiObj(nil, only, multus), "net1", "10.9.0.4", ifaceExact},
		{"single", vmiObj(nil, only), "nic0", "172.31.76.3", ifaceSingle},
		{"primary default", vmiObj(nil, multus, only), "nic0", "172.31.76.3", ifacePrimary},
		{"primary from spec.networks", vmiObj([]any{map[string]any{"name": "net1", "multus": map[string]any{}}, map[string]any{"name": "podnet", "pod": map[string]any{}}},
			multus, map[string]any{"name": "podnet", "ipAddress": "10.0.0.9"}), "nic0", "10.0.0.9", ifacePrimary},
		{"other device on many is not guessed", vmiObj(nil, only, multus), "nic1", "", ifaceNone},
		{"no interfaces yet", vmiObj(nil), "nic0", "", ifaceNone},
	} {
		ip, how, _ := pickVMIInterface(tc.vmi, tc.device)
		if ip != tc.ip || how != tc.how {
			t.Errorf("%s: %q %v, want %q %v", tc.name, ip, how, tc.ip, tc.how)
		}
	}
}

// TestVMIInterfaceIPWarnsOncePerOutcome: an exact match logs nothing; a
// fallback and a miss log WARN with the device and the interface names, once
// until the outcome changes.
func TestVMIInterfaceIPWarnsOncePerOutcome(t *testing.T) {
	ctx := context.Background()
	buf := captureWarn(t)
	k := &kubeOVN{}
	exact := vmiObj(nil, map[string]any{"name": "default", "interfaceName": "nic0", "ipAddress": "10.0.0.5"})
	if ip := k.vmiInterfaceIP(ctx, exact, "nic0"); ip != "10.0.0.5" || buf.Len() != 0 {
		t.Fatalf("exact: %q, log %q", ip, buf.String())
	}
	single := vmiObj(nil, map[string]any{"name": "default", "ipAddress": "172.31.76.3"})
	for range 3 {
		if ip := k.vmiInterfaceIP(ctx, single, "nic0"); ip != "172.31.76.3" {
			t.Fatalf("single: %q", ip)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "device=nic0") || !strings.Contains(buf.String(), "interfaces=[default]") || !strings.Contains(buf.String(), "using=only") {
		t.Fatalf("single: %d warnings: %s", n, buf.String())
	}
	buf.Reset()
	multi := vmiObj(nil, map[string]any{"name": "net1", "ipAddress": "10.9.0.4"}, map[string]any{"name": "default", "ipAddress": "172.31.76.3"})
	if ip := k.vmiInterfaceIP(ctx, multi, "nic0"); ip != "172.31.76.3" || !strings.Contains(buf.String(), "using=\"pod network\"") {
		t.Fatalf("primary: %q, log %q", ip, buf.String())
	}
	buf.Reset()
	for range 3 {
		if ip := k.vmiInterfaceIP(ctx, multi, "nic1"); ip != "" {
			t.Fatalf("miss: %q", ip)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), ifaceMissingWarning) || !strings.Contains(buf.String(), "device=nic1") || !strings.Contains(buf.String(), "interfaces=\"[net1 default]\"") {
		t.Fatalf("miss: %d warnings: %s", n, buf.String())
	}
}
