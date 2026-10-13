//go:build envtest

package network

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestGatewayLabelComesBack is the kube-ovn-controller restart: it sets
// ovn.kubernetes.io/external-gw=false, and the service puts true back on the
// listed nodes only, through a real API server.
func TestEnvtestGatewayLabelComesBack(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	for _, n := range []string{"gw1", "gw2", "worker"} {
		if err := c.Create(ctx, node(n, map[string]string{gatewayLabel: "false"})); err != nil {
			t.Fatal(err)
		}
	}
	label := func(name string) string {
		n := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
			t.Fatal(err)
		}
		return n.Labels[gatewayLabel]
	}
	svc, err := Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("10.78.0.128/25"),
		Workers:      1,
		KubeOVN: KubeOVNConfig{
			Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "external", NADNamespace: envNamespace,
			GatewayNodes: []string{"gw1", "gw2"}, GatewayRecheck: minGatewayRecheck,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	time.Sleep(300 * time.Millisecond)
	if label("gw1") != "false" {
		t.Fatal("labelled before any VPC exists")
	}
	if _, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return label("gw1") == "true" && label("gw2") == "true" })
	// kube-ovn-controller restarts and flips the label.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{gatewayLabel: "false"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, node("gw1", nil), client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return label("gw1") == "true" })
	if label("worker") != "false" {
		t.Fatal("a node not in -kube-gateway-nodes was relabelled")
	}
}

// TestEnvtestGatewayLabelDefaultRecheck leaves GatewayRecheck unset: the
// default period must put the label back within seconds of the reset, which
// is what a kube-ovn-controller eviction or a node reboot needs.
func TestEnvtestGatewayLabelDefaultRecheck(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	if err := c.Create(ctx, node("gw1", map[string]string{gatewayLabel: "true"})); err != nil {
		t.Fatal(err)
	}
	svc, err := Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("10.78.0.128/25"),
		Workers:      1,
		KubeOVN: KubeOVNConfig{
			Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "external", NADNamespace: envNamespace,
			GatewayNodes: []string{"gw1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	if _, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{gatewayLabel: "false"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, node("gw1", nil), client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
	reset := time.Now()
	waitFor(t, func() bool {
		n := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: "gw1"}, n); err != nil {
			t.Fatal(err)
		}
		return n.Labels[gatewayLabel] == "true"
	})
	if d := time.Since(reset); d > defaultGatewayRecheck+2*time.Second {
		t.Fatalf("label came back after %v, want within %v", d, defaultGatewayRecheck)
	}
}
