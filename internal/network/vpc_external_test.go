package network

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnsureVPCJoinsExternalNetwork is vws-test 42 and 43 in code: SNAT only
// worked after the VPC had enableExternal, extraExternalSubnets and a default
// route to the external gateway.
func TestEnsureVPCJoinsExternalNetwork(t *testing.T) {
	ctx := context.Background()
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	for name, tc := range map[string]struct {
		k  func() *kubeOVN
		gw string
	}{
		"operator subnet": {func() *kubeOVN { k, _ := newKube(); return k }, "10.78.0.1"},
		"underlay": {func() *kubeOVN {
			k, _ := newBareKube()
			k.underlay = flatUnderlay()
			return k
		}, "10.30.0.1"},
	} {
		k := tc.k()
		if err := k.ensureVPC(ctx, vpc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		spec := nestedMap(getObj(t, k.c, gvkVpc, "vpc-1").Object, "spec")
		want := map[string]any{
			"enableExternal":       true,
			"extraExternalSubnets": []any{"external"},
			"staticRoutes":         []any{map[string]any{"cidr": "0.0.0.0/0", "nextHopIP": tc.gw, "policy": "policyDst"}},
		}
		if !reflect.DeepEqual(spec, want) {
			t.Fatalf("%s: vpc spec %v, want %v", name, spec, want)
		}
		if err := k.ensureVPC(ctx, vpc); err != nil {
			t.Fatalf("%s: second ensure: %v", name, err)
		}
	}
}

func TestEnsureVPCNeedsExternalGateway(t *testing.T) {
	ctx := context.Background()
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	k, _ := newBareKube()
	if err := k.ensureVPC(ctx, vpc); err == nil || isInvalid(err) || !strings.Contains(err.Error(), "get external subnet external") {
		t.Fatalf("absent external subnet err %v, want a retryable get error", err)
	}
	c := fake.NewClientBuilder().WithObjects(externalSubnetObject("external", "")).Build()
	k = &kubeOVN{c: c, externalSubnet: "external", nadNamespace: "vws-system"}
	if err := k.ensureVPC(ctx, vpc); !isInvalid(err) || !strings.Contains(err.Error(), "spec.gateway") {
		t.Fatalf("gateway-less external subnet err %v, want Invalid", err)
	}
}
