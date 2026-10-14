package network

import (
	"errors"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestCheckVMMTU(t *testing.T) {
	for _, ok := range []int{0, 68, 1500, 8901, 9001} {
		if err := CheckVMMTU(ok); err != nil {
			t.Fatalf("%d: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 67, 9002, 65535} {
		var de *domain.Error
		if err := CheckVMMTU(bad); !errors.As(err, &de) || de.Kind != domain.Invalid {
			t.Fatalf("%d: %v, want Invalid", bad, err)
		}
	}
}

// TestOutOfRangeVMMTUFailsAtStart: the check runs before the dataplane is
// chosen, so a bad -vm-mtu stops the fake dataplane too.
func TestOutOfRangeVMMTUFailsAtStart(t *testing.T) {
	var de *domain.Error
	if _, err := newDataplane(KubeOVNConfig{VMMTU: 9100}); !errors.As(err, &de) || de.Kind != domain.Invalid {
		t.Fatalf("err %v, want Invalid", err)
	}
	if _, err := newDataplane(KubeOVNConfig{}); err != nil {
		t.Fatalf("default: %v", err)
	}
}

func TestSubnetSpecCarriesTheVMMTU(t *testing.T) {
	s := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	spec := (&kubeOVN{nadNamespace: "ns", vmMTU: 8900}).subnetSpec(s)
	if spec["mtu"] != int64(8900) || spec["enableDHCP"] != true || spec["dhcpV4Options"] != "lease_time=3600,mtu=8900" {
		t.Fatalf("spec %v", spec)
	}
	if _, ok := (&kubeOVN{nadNamespace: "ns"}).subnetSpec(s)["mtu"]; ok {
		t.Fatal("mtu set with no vmMTU")
	}
}
