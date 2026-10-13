package network

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func flatUnderlay() UnderlayConfig {
	return UnderlayConfig{ProviderNetwork: "ext", Interface: "ens3", CIDR: mustP("10.30.0.0/24"), Gateway: netip.MustParseAddr("10.30.0.1")}
}

func TestUnderlayValidate(t *testing.T) {
	if err := flatUnderlay().validate(); err != nil {
		t.Fatalf("flat: %v", err)
	}
	tagged := flatUnderlay()
	tagged.VLANID = 4094
	if err := tagged.validate(); err != nil {
		t.Fatalf("vlan 4094: %v", err)
	}
	for want, mut := range map[string]func(*UnderlayConfig){
		"provider network name":  func(u *UnderlayConfig) { u.ProviderNetwork = "" },
		"longer than 12":         func(u *UnderlayConfig) { u.ProviderNetwork = "abcdefghijklm" },
		"node interface":         func(u *UnderlayConfig) { u.Interface = "" },
		"outside 0 (flat)":       func(u *UnderlayConfig) { u.VLANID = 4095 },
		"canonical ipv4":         func(u *UnderlayConfig) { u.CIDR = mustP("10.30.0.5/24") },
		"gateway 10.31.0.1":      func(u *UnderlayConfig) { u.Gateway = netip.MustParseAddr("10.31.0.1") },
		"negative-is-also-error": func(u *UnderlayConfig) { u.VLANID = -1 },
	} {
		u := flatUnderlay()
		mut(&u)
		err := u.validate()
		if !isInvalid(err) {
			t.Fatalf("%s: err %v, want Invalid", want, err)
		}
		if want != "negative-is-also-error" && !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q, want %q", err, want)
		}
	}
}

func TestPoolMustSitInsideUnderlay(t *testing.T) {
	u := flatUnderlay()
	if err := validatePoolInUnderlay(u, mustP("10.30.0.128/25")); err != nil {
		t.Fatal(err)
	}
	if err := validatePoolInUnderlay(UnderlayConfig{}, mustP("10.99.0.0/24")); err != nil {
		t.Fatalf("no underlay: %v", err)
	}
	if err := validatePoolInUnderlay(UnderlayConfig{ProviderNetwork: "ext"}, mustP("10.30.0.0/25")); !isInvalid(err) || !strings.Contains(err.Error(), "node interface") {
		t.Fatalf("incomplete underlay err %v, want the missing field named", err)
	}
	for _, p := range []string{"10.31.0.0/24", "10.30.0.0/23", "10.30.0.0/25"} {
		if err := validatePoolInUnderlay(u, mustP(p)); !isInvalid(err) {
			t.Fatalf("pool %s: err %v, want Invalid", p, err)
		}
	}
}

func TestNewDataplaneUnderlaySelectsAndValidates(t *testing.T) {
	stubInCluster(t, "https://127.0.0.1:1", nil)
	dp, err := newDataplane(KubeOVNConfig{ExternalSubnet: "ext", Underlay: flatUnderlay()})
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := dp.(*kubeOVN); !ok || k.underlay.ProviderNetwork != "ext" {
		t.Fatalf("got %T %+v", dp, dp)
	}
	bad := flatUnderlay()
	bad.Interface = ""
	if _, err := newDataplane(KubeOVNConfig{ExternalSubnet: "ext", Underlay: bad}); !isInvalid(err) {
		t.Fatalf("bad underlay err %v", err)
	}
	if _, err := newDataplane(KubeOVNConfig{Underlay: flatUnderlay()}); !isInvalid(err) {
		t.Fatalf("underlay without external subnet err %v", err)
	}
}

func TestEnsureUnderlayFlatAndVLAN(t *testing.T) {
	ctx := context.Background()
	for _, vlan := range []int{0, 204} {
		k, c := newKube()
		k.underlay = flatUnderlay()
		k.underlay.VLANID = vlan
		for range 2 { // idempotent
			if err := k.ensureUnderlay(ctx); err != nil {
				t.Fatalf("vlan %d: %v", vlan, err)
			}
		}
		pn := get(t, c, gvkProviderNetwork, "ext")
		if nestedString(pn.Object, "spec", "defaultInterface") != "ens3" {
			t.Fatalf("provider network %v", pn.Object)
		}
		vl := get(t, c, gvkVlan, k.underlay.vlanName())
		id, found, err := unstructured.NestedInt64(vl.Object, "spec", "id")
		if err != nil || !found || id != int64(vlan) || nestedString(vl.Object, "spec", "provider") != "ext" {
			t.Fatalf("vlan %v", vl.Object)
		}
		sub := get(t, c, gvkSubnet, "external")
		for f, w := range map[string]string{"cidrBlock": "10.30.0.0/24", "gateway": "10.30.0.1", "vlan": k.underlay.vlanName(), "protocol": "IPv4"} {
			if got := nestedString(sub.Object, "spec", f); got != w {
				t.Fatalf("subnet spec.%s = %q, want %q", f, got, w)
			}
		}
		if err := k.ready(ctx); err != nil {
			t.Fatalf("ready: %v", err)
		}
	}
}

func TestEnsureUnderlayRefusesForeignOrDifferent(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	k.underlay = flatUnderlay()
	seed(t, c, gvkProviderNetwork, "", "ext", "", map[string]any{"defaultInterface": "ens3"}, nil)
	if err := k.ensureUnderlay(ctx); !isConflict(err) {
		t.Fatalf("unannotated provider network err %v, want Conflict", err)
	}
	k, c = newKube()
	k.underlay = flatUnderlay()
	seed(t, c, gvkSubnet, "", "external", "external", map[string]any{"protocol": "IPv4", "cidrBlock": "10.40.0.0/24", "gateway": "10.40.0.1", "vlan": "ext-vlan0"}, nil)
	if err := k.ensureUnderlay(ctx); !isConflict(err) || !strings.Contains(err.Error(), "cidrBlock") {
		t.Fatalf("different external subnet err %v, want Conflict on cidrBlock", err)
	}
}

func TestReadyNeedsProviderNetwork(t *testing.T) {
	k, c := newKube()
	k.underlay = flatUnderlay()
	seed(t, c, gvkSubnet, "", "external", "external", map[string]any{}, nil)
	if err := k.ready(context.Background()); err == nil || !strings.Contains(err.Error(), "provider network ext") {
		t.Fatalf("ready without provider network err %v", err)
	}
}

func get(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestUnderlayExclude(t *testing.T) {
	u := flatUnderlay()
	u.Exclude = []IPRange{{netip.MustParseAddr("10.30.0.1"), netip.MustParseAddr("10.30.0.1")}, {netip.MustParseAddr("10.30.0.2"), netip.MustParseAddr("10.30.0.9")}}
	if err := u.validate(); err != nil {
		t.Fatal(err)
	}
	got := u.excludeIPs()
	if len(got) != 2 || got[0] != "10.30.0.1" || got[1] != "10.30.0.2..10.30.0.9" {
		t.Fatalf("excludeIps %v", got)
	}
	if err := validatePoolInUnderlay(u, mustP("10.30.0.128/25")); err != nil {
		t.Fatal(err)
	}
	if err := validatePoolInUnderlay(u, mustP("10.30.0.8/29")); !isInvalid(err) || !strings.Contains(err.Error(), "exclude") {
		t.Fatalf("pool over exclude err %v", err)
	}
	for _, r := range []string{"10.30.0.9..10.30.0.2", "10.31.0.5", "10.30.0.250..10.30.1.2"} {
		bad := flatUnderlay()
		x, err := ParseIPRange(r)
		if err != nil {
			t.Fatal(err)
		}
		bad.Exclude = []IPRange{x}
		if err := bad.validate(); !isInvalid(err) {
			t.Fatalf("exclude %s err %v, want Invalid", r, err)
		}
	}
	k, c := newKube()
	k.underlay = u
	if err := k.ensureUnderlay(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex, _, err := unstructured.NestedStringSlice(get(t, c, gvkSubnet, "external").Object, "spec", "excludeIps")
	if err != nil || len(ex) != 2 || ex[0] != "10.30.0.1" {
		t.Fatalf("subnet excludeIps %v %v", ex, err)
	}
}

func TestEnsureVPCAttachesExternalSubnet(t *testing.T) {
	k, c := newKube()
	if err := k.ensureVPC(context.Background(), domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	got, _, err := unstructured.NestedStringSlice(get(t, c, gvkVpc, "vpc-1").Object, "spec", "extraExternalSubnets")
	if err != nil || len(got) != 1 || got[0] != "external" {
		t.Fatalf("extraExternalSubnets %v %v", got, err)
	}
}
