package grpcapi

import (
	"testing"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"
)

func TestICMPTypeCodeRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		from, to int32
	}{
		{"echo reply", 0, 0},
		{"all icmp", -1, -1},
		{"echo request, all codes", 8, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, err := parseRules([]*networkv1.SecurityGroupRule{{
				Id:        "icmp",
				Direction: networkv1.Direction_DIRECTION_INGRESS,
				Protocol:  networkv1.Protocol_PROTOCOL_ICMP,
				FromPort:  c.from,
				ToPort:    c.to,
				Target:    &networkv1.SecurityGroupRule_Cidr{Cidr: "0.0.0.0/0"},
			}}, "proj", "r1", "app")
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) != 1 || rules[0].FromPort != c.from || rules[0].ToPort != c.to {
				t.Fatalf("parsed %+v", rules)
			}
			got := toRule("proj", "r1", "app", rules[0])
			if got.GetFromPort() != c.from || got.GetToPort() != c.to {
				t.Fatalf("serialized from=%d to=%d", got.GetFromPort(), got.GetToPort())
			}
		})
	}
}
