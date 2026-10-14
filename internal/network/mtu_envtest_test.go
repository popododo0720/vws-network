//go:build envtest

package network

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestSubnetMTU writes VPC subnets through the real v1.16.12 schema
// with strictClient (a pruned field fails the test) and reads spec.mtu and
// spec.enableDHCP back: the 9001 default and a value an installer lowered.
func TestEnvtestSubnetMTU(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	if err := strictKube(t, c, false).ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	for i, mtu := range []int{DefaultVMMTU, 1450} {
		k := strictKube(t, c, false)
		k.vmMTU = mtu
		s := domain.Subnet{ID: domain.SubnetID([]string{"subnet-default", "subnet-lowered"}[i]), VPC: "vpc-1", CIDR: mustP([]string{"10.0.1.0/24", "10.0.2.0/24"}[i])}
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
		o := getObj(t, c, gvkSubnet, string(s.ID))
		got, _, err := unstructuredInt(o.Object, "spec", "mtu")
		if err != nil || got != int64(mtu) {
			t.Fatalf("%s: spec.mtu %d %v, want %d", s.ID, got, err, mtu)
		}
		if dhcp, ok, err := unstructured.NestedBool(o.Object, "spec", "enableDHCP"); err != nil || !ok || !dhcp {
			t.Fatalf("%s: spec.enableDHCP not true: %v", s.ID, o.Object["spec"])
		}
		if opts := nestedString(o.Object, "spec", "dhcpV4Options"); opts != fmt.Sprintf("lease_time=3600,mtu=%d", mtu) {
			t.Fatalf("%s: spec.dhcpV4Options %q", s.ID, opts)
		}
	}
}

func unstructuredInt(obj map[string]any, fields ...string) (int64, bool, error) {
	v, ok, err := unstructured.NestedInt64(obj, fields...)
	if err != nil {
		return 0, false, fmt.Errorf("nested int: %w", err)
	}
	return v, ok, nil
}
