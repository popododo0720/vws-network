package network

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestOvnRule(t *testing.T) {
	anywhere := domain.Peer{CIDR: mustP("0.0.0.0/0")}
	cases := []struct {
		name string
		in   domain.Rule
		want map[string]any
	}{
		{
			"tcp port range from a cidr",
			domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 80, Peer: domain.Peer{CIDR: mustP("10.0.0.0/8")}},
			map[string]any{"ipVersion": "ipv4", "priority": int64(1), "policy": "allow", "protocol": "tcp", "portRangeMin": int64(22), "portRangeMax": int64(80), "remoteType": "address", "remoteAddress": "10.0.0.0/8"},
		},
		{
			"udp from a group",
			domain.Rule{Direction: domain.Ingress, Protocol: domain.UDP, FromPort: 53, ToPort: 53, Peer: domain.Peer{Group: "sg-peer"}},
			map[string]any{"ipVersion": "ipv4", "priority": int64(1), "policy": "allow", "protocol": "udp", "portRangeMin": int64(53), "portRangeMax": int64(53), "remoteType": "securityGroup", "remoteSecurityGroup": "sg-peer"},
		},
		{
			"icmp has no ports",
			domain.Rule{Direction: domain.Ingress, Protocol: domain.ICMP, FromPort: -1, ToPort: -1, Peer: anywhere},
			map[string]any{"ipVersion": "ipv4", "priority": int64(1), "policy": "allow", "protocol": "icmp", "remoteType": "address", "remoteAddress": "0.0.0.0/0"},
		},
		{
			"default egress",
			domain.DefaultEgress(),
			map[string]any{"ipVersion": "ipv4", "priority": int64(1), "policy": "allow", "protocol": "all", "remoteType": "address", "remoteAddress": "0.0.0.0/0"},
		},
	}
	for _, c := range cases {
		got, err := ovnRule(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v err %v, want %v", c.name, got, err, c.want)
		}
	}
}

func TestOvnRuleRejectsUnsupported(t *testing.T) {
	cases := map[string]domain.Rule{
		"tcp port 0":     {Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 0, ToPort: 10, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}},
		"no direction":   {Protocol: domain.All, FromPort: -1, ToPort: -1, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}},
		"cidr and group": {Direction: domain.Ingress, Protocol: domain.All, FromPort: -1, ToPort: -1, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0"), Group: "sg-1"}},
	}
	for name, r := range cases {
		if _, err := ovnRule(r); !isInvalid(err) {
			t.Errorf("%s: err %v, want Invalid", name, err)
		}
	}
}

func groupRules(t *testing.T, k *kubeOVN, id, field string) []any {
	t.Helper()
	o := getObj(t, k.c, gvkGroup, id)
	rules, _, err := unstructured.NestedSlice(o.Object, "spec", field)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestKubeOVNRulesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	k, _ := newKube()
	g := domain.SecurityGroup{ID: "sg-1"}
	ssh := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	for range 2 {
		if err := k.ensureGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
		if err := k.ensureRule(ctx, "sg-1", ssh); err != nil {
			t.Fatal(err)
		}
		if err := k.ensureRule(ctx, "sg-1", domain.DefaultEgress()); err != nil {
			t.Fatal(err)
		}
	}
	if got := groupRules(t, k, "sg-1", "ingressRules"); len(got) != 1 {
		t.Fatalf("ingress %v", got)
	}
	if got := groupRules(t, k, "sg-1", "egressRules"); len(got) != 1 {
		t.Fatalf("egress %v", got)
	}
	for range 2 {
		if err := k.deleteRule(ctx, "sg-1", ssh); err != nil {
			t.Fatal(err)
		}
	}
	if got := groupRules(t, k, "sg-1", "ingressRules"); len(got) != 0 {
		t.Fatalf("ingress after delete %v", got)
	}
	if got := groupRules(t, k, "sg-1", "egressRules"); len(got) != 1 {
		t.Fatalf("delete touched egress: %v", got)
	}
}

func TestKubeOVNGroupDeleteAndMissingGroup(t *testing.T) {
	ctx := context.Background()
	k, _ := newKube()
	r := domain.DefaultEgress()
	if err := k.deleteRule(ctx, "sg-9", r); err != nil {
		t.Fatalf("delete rule in absent group: %v", err)
	}
	if err := k.ensureRule(ctx, "sg-9", r); err == nil {
		t.Fatal("ensure rule in absent group succeeded")
	}
	if err := k.deleteGroup(ctx, domain.SecurityGroup{ID: "sg-9"}); err != nil {
		t.Fatalf("delete absent group: %v", err)
	}
}
