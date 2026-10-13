//go:build envtest

package network

import (
	"context"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestSNATIsOVNNative writes the SNAT objects through the real v1.16.12
// schema and checks that no VpcNatGateway is made.
func TestEnvtestSNATIsOVNNative(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := strictKube(t, c, true)
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	if err := k.ensureSubnet(ctx, sub); err != nil {
		t.Fatal(err)
	}
	getObj(t, c, gvkSnat, "snat-subnet-1")
	getObj(t, c, gvkEip, "snat-subnet-1")
	noNatGateways(t, c)
	if err := k.deleteSubnet(ctx, sub); err != nil {
		t.Fatal(err)
	}
	absent(t, c, "snat-subnet-1", "rule")
	absent(t, c, "snat-subnet-1", "eip")
}
