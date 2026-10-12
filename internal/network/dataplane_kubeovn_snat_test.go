package network

import (
	"context"
	"net/netip"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestNatLanIP(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.1.0/24":  "10.0.1.254",
		"10.0.1.77/24": "10.0.1.254",
		"10.0.0.16/28": "10.0.0.30",
		"10.0.0.0/16":  "10.0.255.254",
	} {
		if got := natLanIP(netip.MustParsePrefix(in)).String(); got != want {
			t.Errorf("natLanIP(%s) = %s, want %s", in, got, want)
		}
	}
}

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
	gw := getObj(t, c, gvkNatGW, "nat-vpc-1")
	spec, _, _ := unstructured.NestedMap(gw.Object, "spec")
	if spec["vpc"] != "vpc-1" || spec["subnet"] != "subnet-a" || spec["lanIp"] != "10.0.1.254" {
		t.Fatalf("gateway spec %v", spec)
	}
	if ext, _ := spec["externalSubnets"].([]any); len(ext) != 1 || ext[0] != "external" {
		t.Fatalf("gateway external subnets %v", spec["externalSubnets"])
	}
	if gw.GetAnnotations()[annotationID] != "nat-vpc-1" {
		t.Fatalf("gateway annotations %v", gw.GetAnnotations())
	}
	for _, id := range []string{"subnet-a", "subnet-b"} {
		rule := getObj(t, c, gvkSnat, "snat-"+id)
		rs, _, _ := unstructured.NestedStringMap(rule.Object, "spec")
		if rs["ovnEip"] != "snat-"+id || rs["vpcSubnet"] != id {
			t.Fatalf("rule spec %v", rs)
		}
		eip := getObj(t, c, gvkEip, "snat-"+id)
		es, _, _ := unstructured.NestedStringMap(eip.Object, "spec")
		if es["externalSubnet"] != "external" || es["type"] != "nat" || es["v4ip"] != "" {
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
	for range 2 {
		if err := k.deleteSubnet(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	absent(t, c, "snat-subnet-b", "rule")
	absent(t, c, "snat-subnet-b", "eip")
	getObj(t, c, gvkNatGW, "nat-vpc-1")
	for range 2 {
		if err := k.deleteSubnet(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	absent(t, c, "snat-subnet-a", "rule")
	absent(t, c, "snat-subnet-a", "eip")
	absent(t, c, "nat-vpc-1", "gw")
}

func TestKubeOVNSNATNeedsExternalSubnet(t *testing.T) {
	k, c := snatKube()
	k.externalSubnet = ""
	err := k.ensureSubnet(context.Background(), domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")})
	if !isInvalid(err) {
		t.Fatalf("err %v, want Invalid", err)
	}
	absent(t, c, "nat-vpc-1", "gw")
}

func TestKubeOVNSubnetWithoutSNAT(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	if err := k.ensureSubnet(ctx, domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}); err != nil {
		t.Fatal(err)
	}
	absent(t, c, "nat-vpc-1", "gw")
	absent(t, c, "snat-subnet-a", "rule")
}

func gatewayHost(t *testing.T, c client.Client) (string, string) {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(getObj(t, c, gvkNatGW, "nat-vpc-1").Object, "spec")
	host, _ := spec["subnet"].(string)
	lan, _ := spec["lanIp"].(string)
	return host, lan
}

func TestKubeOVNDeletingGatewayHostMovesGateway(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	other := domain.Subnet{ID: "subnet-0", VPC: "vpc-2", CIDR: mustP("10.1.0.0/24")}
	for _, s := range []domain.Subnet{a, b, other} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := k.deleteSubnet(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if host, lan := gatewayHost(t, c); host != "subnet-b" || lan != "10.0.2.254" {
		t.Fatalf("gateway on %s %s, want subnet-b 10.0.2.254", host, lan)
	}
	getObj(t, c, gvkSnat, "snat-subnet-b")
	getObj(t, c, gvkNatGW, "nat-vpc-2")
	if err := k.deleteSubnet(ctx, b); err != nil {
		t.Fatal(err)
	}
	absent(t, c, "nat-vpc-1", "gw")
	getObj(t, c, gvkNatGW, "nat-vpc-2")
}

func TestKubeOVNDeleteHostRecreatesLostGateway(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	for _, s := range []domain.Subnet{a, b} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.remove(ctx, gvkNatGW, "nat-vpc-1"); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteSubnet(ctx, a); err != nil {
		t.Fatal(err)
	}
	if host, _ := gatewayHost(t, c); host != "subnet-b" {
		t.Fatalf("gateway on %s, want subnet-b", host)
	}
}

func TestKubeOVNDeleteHostWaitsForTerminatingGateway(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	for _, s := range []domain.Subnet{a, b} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	gw := getObj(t, c, gvkNatGW, "nat-vpc-1")
	gw.SetFinalizers([]string{"vws.test/hold"})
	if err := c.Update(ctx, gw); err != nil {
		t.Fatal(err)
	}
	err := k.deleteSubnet(ctx, a)
	if err == nil || isInvalid(err) || isConflict(err) {
		t.Fatalf("err %v, want a retryable error", err)
	}
	gw = getObj(t, c, gvkNatGW, "nat-vpc-1")
	gw.SetFinalizers(nil)
	if err := c.Update(ctx, gw); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteSubnet(ctx, a); err != nil {
		t.Fatal(err)
	}
	if host, _ := gatewayHost(t, c); host != "subnet-b" {
		t.Fatalf("gateway on %s, want subnet-b", host)
	}
}

func TestKubeOVNRehomeKeepsForeignGateway(t *testing.T) {
	ctx := context.Background()
	k, c := snatKube()
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	for _, s := range []domain.Subnet{a, b} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	gw := getObj(t, c, gvkNatGW, "nat-vpc-1")
	gw.SetAnnotations(map[string]string{annotationID: "someone-else"})
	if err := c.Update(ctx, gw); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteSubnet(ctx, a); !isConflict(err) {
		t.Fatalf("err %v, want conflict", err)
	}
	if host, _ := gatewayHost(t, c); host != "subnet-a" {
		t.Fatalf("foreign gateway moved to %s", host)
	}
}
