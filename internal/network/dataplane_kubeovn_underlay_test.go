package network

import (
	"net/netip"
	"strings"
	"testing"
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
