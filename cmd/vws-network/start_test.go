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
