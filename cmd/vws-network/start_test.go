package main

import (
	"strings"
	"testing"
)

func TestParseStartPoolRequired(t *testing.T) {
	for _, tc := range []struct{ db, pool, want string }{
		{"", "10.20.0.0/24", "-db is required"},
		{"x.db", "", "-pool is required"},
		{"x.db", "nope", "-pool \"nope\""},
		{"x.db", "192.0.2.0/24", "documentation range 192.0.2.0/24"},
		{"x.db", "198.51.100.128/25", "documentation range"},
		{"x.db", "203.0.113.0/24", "documentation range"},
		{"x.db", "10.20.0.1/24", "not canonical"},
	} {
		_, err := parseStart(tc.db, tc.pool)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseStart(%q, %q) = %v, want %q", tc.db, tc.pool, err, tc.want)
		}
	}
	p, err := parseStart("x.db", "10.20.0.0/24")
	if err != nil || p.String() != "10.20.0.0/24" {
		t.Fatalf("valid pool: %v %v", p, err)
	}
}

func TestParseUnderlay(t *testing.T) {
	u, err := parseUnderlay("ext", "ens3", 204, "10.30.0.0/24", "10.30.0.1", "")
	if err != nil || u.CIDR.String() != "10.30.0.0/24" || u.Gateway.String() != "10.30.0.1" || u.VLANID != 204 {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := parseUnderlay("ext", "ens3", 0, "bad", "", ""); err == nil || !strings.Contains(err.Error(), "-kube-underlay-cidr") {
		t.Fatalf("bad cidr err %v", err)
	}
	if _, err := parseUnderlay("ext", "ens3", 0, "", "bad", ""); err == nil || !strings.Contains(err.Error(), "-kube-underlay-gateway") {
		t.Fatalf("bad gateway err %v", err)
	}
}

func TestParseUnderlayExclude(t *testing.T) {
	u, err := parseUnderlay("vws-pn0", "ens3", 0, "10.30.0.0/24", "10.30.0.1", " 10.30.0.2..10.30.0.9, 10.30.0.20 ,")
	if err != nil || len(u.Exclude) != 2 || u.Exclude[0].String() != "10.30.0.2..10.30.0.9" || u.Exclude[1].String() != "10.30.0.20" {
		t.Fatalf("%+v %v", u.Exclude, err)
	}
	if _, err := parseUnderlay("vws-pn0", "ens3", 0, "", "", "10.30.0.2..x"); err == nil || !strings.Contains(err.Error(), "-kube-underlay-exclude") {
		t.Fatalf("bad exclude err %v", err)
	}
}

func TestParseDirect(t *testing.T) {
	if got, err := parseDirect(" "); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	got, err := parseDirect(`[{"name":"prod","providerNetwork":"vws-pn0","interface":"ens3","vlan":200,"cidr":"192.168.200.0/24","gateway":"192.168.200.1","exclude":["192.168.200.2..192.168.200.20"]}]`)
	if err != nil || len(got) != 1 || got[0].Name != "prod" || got[0].VLANID != 200 || got[0].CIDR.String() != "192.168.200.0/24" || len(got[0].Exclude) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{`[{"name":"x","vlanId":1}]`, `{"name":"x"}`, `[] []`, `[{"cidr":"nope"}]`} {
		if _, err := parseDirect(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}
