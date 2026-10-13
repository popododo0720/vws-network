//go:build envtest

package network

import (
	"context"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestVPCExternalFieldsKept writes the VPC through the real v1.16.12
// schema, so enableExternal, extraExternalSubnets and staticRoutes must be
// field names Kube-OVN knows.
func TestEnvtestVPCExternalFieldsKept(t *testing.T) {
	_, c := startEnv(t)
	k := strictKube(t, c, false)
	if err := k.ensureVPC(context.Background(), domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	routes := nestedSlice(getObj(t, c, gvkVpc, "vpc-1").Object, "spec", "staticRoutes")
	if len(routes) != 1 {
		t.Fatalf("stored routes %v", routes)
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["nextHopIP"] != "10.78.0.1" || route["policy"] != "policyDst" {
		t.Fatalf("stored route %v", routes[0])
	}
}
