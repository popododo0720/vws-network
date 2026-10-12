//go:build envtest

package network

import (
	"context"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestEnvtestRemoveKeepsForeignObject(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := envKube(c, true)
	seed(t, c, gvkVpc, "", "vpc-foreign", "", map[string]any{}, nil)
	seed(t, c, gvkNAD, envNamespace, "subnet-foreign", "other", map[string]any{}, nil)
	if err := k.deleteVPC(ctx, domain.VPC{ID: "vpc-foreign"}); !isConflict(err) {
		t.Fatalf("deleteVPC err = %v, want conflict", err)
	}
	if err := k.removeNAD(ctx, "subnet-foreign"); !isConflict(err) {
		t.Fatalf("removeNAD err = %v, want conflict", err)
	}
	if !exists(c, gvkVpc, "", "vpc-foreign") || !exists(c, gvkNAD, envNamespace, "subnet-foreign") {
		t.Fatal("a foreign object was deleted")
	}

	vpc := domain.VPC{ID: "vpc-own", CIDR: mustP("10.0.0.0/16")}
	if err := k.ensureVPC(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteVPC(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	if exists(c, gvkVpc, "", "vpc-own") {
		t.Fatal("owned VPC was kept")
	}
}
