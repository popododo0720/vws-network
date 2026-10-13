package network

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

func prodDirect() DirectNetwork {
	return DirectNetwork{Name: "prod", ProviderNetwork: "ext", Interface: "ens3", VLANID: 200, CIDR: mustP("192.168.200.0/24"), Gateway: netip.MustParseAddr("192.168.200.1"), Exclude: []string{"192.168.200.2..192.168.200.20"}}
}

func TestDirectNetworkJSONContract(t *testing.T) {
	var got []DirectNetwork
	line := `[{"name":"prod","providerNetwork":"ext","interface":"ens3","vlan":200,"cidr":"192.168.200.0/24","gateway":"192.168.200.1","exclude":["192.168.200.2..192.168.200.20"]}]`
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	want := prodDirect()
	if len(got) != 1 || got[0].Name != want.Name || got[0].CIDR != want.CIDR || got[0].Gateway != want.Gateway || got[0].VLANID != 200 || got[0].Exclude[0] != want.Exclude[0] {
		t.Fatalf("%+v", got)
	}
}

func TestValidateDirect(t *testing.T) {
	ext := flatUnderlay() // ext on ens3, untagged, 10.30.0.0/24
	pool := mustP("10.30.0.128/25")
	if err := validateDirect([]DirectNetwork{prodDirect()}, ext, pool); err != nil {
		t.Fatalf("shared provider network: %v", err)
	}
	other := prodDirect()
	other.Name, other.ProviderNetwork, other.Interface, other.VLANID = "lab", "lab", "ens4", 0
	other.CIDR, other.Gateway, other.Exclude = mustP("192.168.10.0/24"), netip.MustParseAddr("192.168.10.1"), nil
	if err := validateDirect([]DirectNetwork{prodDirect(), other}, ext, pool); err != nil {
		t.Fatalf("second NIC: %v", err)
	}
	if err := validateDirect([]DirectNetwork{prodDirect()}, UnderlayConfig{}, pool); err != nil {
		t.Fatalf("no external underlay: %v", err)
	}
	for want, mut := range map[string]func(*DirectNetwork){
		"must match":                      func(d *DirectNetwork) { d.Name = "Prod" },
		"one provider network is one NIC": func(d *DirectNetwork) { d.Interface = "ens4" },
		"one provider network per NIC":    func(d *DirectNetwork) { d.ProviderNetwork = "other" },
		"both on ens3 vlan 0":             func(d *DirectNetwork) { d.VLANID = 0 },
		"overlaps the external network": func(d *DirectNetwork) {
			d.CIDR, d.Gateway, d.Exclude = mustP("10.30.0.0/25"), netip.MustParseAddr("10.30.0.1"), nil
		},
		"gateway": func(d *DirectNetwork) { d.Gateway = netip.MustParseAddr("10.0.0.1") },
		"exclude": func(d *DirectNetwork) { d.Exclude = []string{"x"} },
	} {
		d := prodDirect()
		mut(&d)
		err := validateDirect([]DirectNetwork{d}, ext, pool)
		if !isInvalid(err) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err %v", want, err)
		}
	}
	if err := validateDirect([]DirectNetwork{prodDirect(), prodDirect()}, ext, pool); !isInvalid(err) || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("duplicate err %v", err)
	}
	overPool := prodDirect()
	if err := validateDirect([]DirectNetwork{overPool}, UnderlayConfig{}, mustP("192.168.200.0/25")); !isInvalid(err) || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("pool overlap err %v", err)
	}
}

func TestDirectSubnetsView(t *testing.T) {
	subs := directSubnets([]DirectNetwork{prodDirect()})
	if len(subs) != 1 || subs[0].ID != "subnet-direct-prod" || subs[0].VPC != DirectVPC || subs[0].CIDR != mustP("192.168.200.0/24") {
		t.Fatalf("%+v", subs)
	}
}

func TestServiceDirectNetworksAreSharedAndReadOnly(t *testing.T) {
	svc, _ := openTest(t, newMemRepo())
	svc.direct = directSubnets([]DirectNetwork{prodDirect()})
	ctx := context.Background()
	for _, p := range []domain.ProjectID{"proj", "other"} {
		got, err := svc.GetSubnet(ctx, p, "subnet-direct-prod")
		if err != nil || got.VPC != DirectVPC {
			t.Fatalf("%s get direct: %+v %v", p, got, err)
		}
		for _, vpc := range []domain.VPCID{"", DirectVPC} {
			subs, err := svc.ListSubnets(ctx, p, vpc)
			if err != nil || len(subs) != 1 || subs[0].ID != "subnet-direct-prod" {
				t.Fatalf("%s list %q: %+v %v", p, vpc, subs, err)
			}
		}
	}
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.9.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(vpcOp.Resource)
	if subs, _ := svc.ListSubnets(ctx, "proj", vpc); len(subs) != 0 {
		t.Fatalf("a tenant vpc lists %+v", subs)
	}
	forbidden := map[string]func() error{
		"create in vpc-direct": func() error {
			_, err := svc.CreateSubnet(ctx, call("proj", "c1"), domain.CreateSubnet{VPC: DirectVPC, CIDR: mustP("192.168.200.0/25"), Zone: "az1"})
			return err
		},
		"create a direct id": func() error {
			_, err := svc.CreateSubnet(ctx, call("proj", "c2"), domain.CreateSubnet{ID: "subnet-direct-x", VPC: vpc, CIDR: mustP("10.9.0.0/24"), Zone: "az1"})
			return err
		},
		"update": func() error {
			_, err := svc.UpdateSubnet(ctx, call("proj", "u"), domain.CreateSubnet{ID: "subnet-direct-prod", VPC: DirectVPC, Keep: true})
			return err
		},
		"delete": func() error {
			_, err := svc.DeleteSubnet(ctx, call("proj", "d"), domain.DeleteSubnet{Subnet: "subnet-direct-prod"})
			return err
		},
		"create vpc-direct": func() error {
			_, err := svc.CreateVPC(ctx, call("proj", "v"), domain.CreateVPC{ID: DirectVPC, CIDR: mustP("10.10.0.0/16")})
			return err
		},
	}
	for name, f := range forbidden {
		if err := f(); kind(err) != domain.Forbidden || !strings.Contains(err.Error(), "UnauthorizedOperation") {
			t.Fatalf("%s err %v, want Forbidden", name, err)
		}
	}
	if _, err := svc.AllocateElasticIP(ctx, call("proj", "e0"), domain.AllocateElasticIP{VPC: DirectVPC}); !isInvalid(err) || !strings.Contains(err.Error(), "InvalidParameterCombination") {
		t.Fatalf("eip in vpc-direct err %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpc, CIDR: mustP("10.9.0.0/24"), Zone: "az1"}); err != nil {
		t.Fatal(err)
	}
	eip, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: vpc})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	id := domain.ElasticIPID(eip.Resource)
	if _, err := svc.AssociateElasticIP(ctx, call("proj", "a1"), domain.AssociateElasticIP{ElasticIP: id, Private: netip.MustParseAddr("192.168.200.30")}); !isInvalid(err) || !strings.Contains(err.Error(), "InvalidParameterCombination") {
		t.Fatalf("eip on a direct eni err %v", err)
	}
	if _, err := svc.AssociateElasticIP(ctx, call("proj", "a2"), domain.AssociateElasticIP{ElasticIP: id, Private: netip.MustParseAddr("10.9.0.5")}); err != nil {
		t.Fatalf("eip on a tenant eni: %v", err)
	}
}
