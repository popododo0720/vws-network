package network

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func nodeLabel(t *testing.T, c client.Client, name string) (string, bool) {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	v, ok := n.Labels[gatewayLabel]
	return v, ok
}

func TestLabelGatewayNodesRestoresOnlyListedNodes(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(
		node("n1", map[string]string{gatewayLabel: "false", "keep": "me"}),
		node("n2", nil),
		node("n3", map[string]string{gatewayLabel: "true"}),
		node("other", map[string]string{gatewayLabel: "false"}),
	).Build()
	k := &kubeOVN{c: c, gatewayNodes: []string{"n1", "n2", "n3", "gone"}}
	fixed, err := k.labelGatewayNodes(context.Background())
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("missing node err %v, want NotFound for gone", err)
	}
	if len(fixed) != 2 || fixed[0] != "n1" || fixed[1] != "n2" {
		t.Fatalf("fixed %v, want [n1 n2]", fixed)
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		if v, _ := nodeLabel(t, c, n); v != "true" {
			t.Fatalf("%s label %q", n, v)
		}
	}
	if v, _ := nodeLabel(t, c, "other"); v != "false" {
		t.Fatalf("unlisted node changed to %q", v)
	}
	n1 := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "n1"}, n1); err != nil || n1.Labels["keep"] != "me" {
		t.Fatalf("other labels lost: %v %v", n1.Labels, err)
	}
	fixed, err = k.labelGatewayNodes(context.Background())
	if len(fixed) != 0 || !apierrors.IsNotFound(err) {
		t.Fatalf("second pass fixed %v err %v", fixed, err)
	}
}

func TestRelabelGatewaysWaitsForAVPC(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(node("n1", map[string]string{gatewayLabel: "false"})).Build()
	k := &kubeOVN{c: c, gatewayNodes: []string{"n1"}}
	svc, err := newService(ctx, newMemRepo(), newFake(), mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	svc.relabelGateways(ctx, k)
	if v, _ := nodeLabel(t, c, "n1"); v != "false" {
		t.Fatalf("labelled with no VPC: %q", v)
	}
	if _, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	svc.relabelGateways(ctx, k)
	if v, _ := nodeLabel(t, c, "n1"); v != "true" {
		t.Fatalf("label after a VPC: %q", v)
	}
}

func TestGatewayNodesSelectKubeOVN(t *testing.T) {
	if !(KubeOVNConfig{GatewayNodes: []string{"n1"}}).selected() {
		t.Fatal("-kube-gateway-nodes alone must select Kube-OVN")
	}
}

func TestGatewayLabelUnkept(t *testing.T) {
	if !gatewayLabelUnkept(&kubeOVN{externalSubnet: "external"}) {
		t.Fatal("no warning for an external network without gateway nodes")
	}
	if gatewayLabelUnkept(&kubeOVN{externalSubnet: "external", gatewayNodes: []string{"gw1"}}) {
		t.Fatal("warning with gateway nodes")
	}
	if gatewayLabelUnkept(&kubeOVN{}) {
		t.Fatal("warning without an external network")
	}
}
