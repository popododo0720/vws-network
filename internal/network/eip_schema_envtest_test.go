//go:build envtest

package network

import (
	"context"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestElasticIPKeepsAddress writes an OvnEip and OvnFip through the real
// v1.16.12 schema. With v4ip (lower case) the server pruned the address and
// Kube-OVN picked another one (vws-test 38, 38b: API 10.78.0.225, OvnEip 10.78.0.201).
func TestEnvtestElasticIPKeepsAddress(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := strictKube(t, c, false)
	e := domain.ElasticIP{ID: "eipalloc-1", VPC: "vpc-1", Address: mustP("10.78.0.225/32").Addr()}
	if err := k.ensureElasticIP(ctx, e); err != nil {
		t.Fatal(err)
	}
	if got := nestedString(getObj(t, c, gvkEip, "eipalloc-1").Object, "spec", "v4Ip"); got != "10.78.0.225" {
		t.Fatalf("stored spec.v4Ip %q, want the address the API returned", got)
	}
	if err := k.ensureElasticIP(ctx, e); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if err := k.ensureNAT(ctx, e, mustP("10.0.1.5/32").Addr()); err != nil {
		t.Fatal(err)
	}
}
