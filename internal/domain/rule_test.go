package domain

import "testing"

func TestCheckRuleICMPTypeAndCode(t *testing.T) {
	peer := Peer{CIDR: mustPrefix(t, "0.0.0.0/0")}
	cases := []struct {
		name     string
		from, to int32
		ok       bool
	}{
		{"echo reply", 0, 0, true},
		{"all", -1, -1, true},
		{"echo request, all codes", 8, -1, true},
		{"all types, one code", -1, 0, false},
		{"type too large", 256, 0, false},
		{"code too large", 0, 256, false},
		{"negative type", -2, -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckRule(Rule{Direction: Ingress, Protocol: ICMP, FromPort: c.from, ToPort: c.to, Peer: peer})
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok %v", err, c.ok)
			}
		})
	}
}
