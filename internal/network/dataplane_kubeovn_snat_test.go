package network

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// gvkNatGW is only here to prove nothing makes one: SNAT is OVN-native.
var gvkNatGW = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "VpcNatGateway"}

func snatKube() (*kubeOVN, client.Client) {
	k, c := newKube()
	k.snat = true
	return k, c
}

func absent(t *testing.T, c client.Client, name, kind string) {
	t.Helper()
	o := &unstructured.Unstructured{}
	switch kind {
	case "rule":
		o.SetGroupVersionKind(gvkSnat)
	case "eip":
		o.SetGroupVersionKind(gvkEip)
	default:
		o.SetGroupVersionKind(gvkNatGW)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, o); !apierrors.IsNotFound(err) {
		t.Fatalf("%s %s still present: %v", kind, name, err)
	}
}

func noNatGateways(t *testing.T, c client.Client) {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvkNatGW.GroupVersion().WithKind("VpcNatGatewayList"))
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("%d VpcNatGateway objects, want none", len(list.Items))
	}
}

func TestKubeOVNSubnetSNAT(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	for range 2 {
		for _, s := range []domain.Subnet{a, b} {
			if err := k.ensureSubnet(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	noNatGateways(t, c)
	for _, id := range []string{"subnet-a", "subnet-b"} {
		rule := getObj(t, c, gvkSnat, "snat-"+id)
		rs := nestedStringMap(rule.Object, "spec")
		if rs["ovnEip"] != "snat-"+id || rs["vpcSubnet"] != id {
			t.Fatalf("rule spec %v", rs)
		}
		eip := getObj(t, c, gvkEip, "snat-"+id)
		es := nestedStringMap(eip.Object, "spec")
		if es["externalSubnet"] != "external" || es["type"] != "nat" || es["v4Ip"] != "" {
			t.Fatalf("eip spec %v", es)
		}
	}
}

func TestKubeOVNDeleteSubnetSNAT(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	for _, s := range []domain.Subnet{a, b} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []domain.Subnet{b, a} {
		for range 2 {
			if err := k.deleteSubnet(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		absent(t, c, snatName(s.ID), "rule")
		absent(t, c, snatName(s.ID), "eip")
	}
	noNatGateways(t, c)
}

// TestKubeOVNDeleteSNATLeavesOldGateway: a VpcNatGateway made by an older
// release is not touched; the operator removes it (docs/kube-ovn-conflict-recovery.md).
func TestKubeOVNDeleteSNATLeavesOldGateway(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	seed(t, c, gvkNatGW, "", "nat-vpc-1", "nat-vpc-1", map[string]any{"subnet": "subnet-1"}, nil)
	if err := k.deleteSNAT(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if !exists(c, gvkNatGW, "", "nat-vpc-1") {
		t.Fatal("old gateway was deleted")
	}
}

func TestKubeOVNSNATNeedsExternalSubnet(t *testing.T) {
	k, c := snatKube()
	k.externalSubnet = ""
	err := k.ensureSubnet(context.Background(), domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")})
	if !isInvalid(err) {
		t.Fatalf("err %v, want Invalid", err)
	}
	absent(t, c, "snat-subnet-a", "eip")
}

func TestKubeOVNSubnetWithoutSNAT(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	if err := k.ensureSubnet(ctx, domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}); err != nil {
		t.Fatal(err)
	}
	absent(t, c, "snat-subnet-a", "rule")
	absent(t, c, "snat-subnet-a", "eip")
}
