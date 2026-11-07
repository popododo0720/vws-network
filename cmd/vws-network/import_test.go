package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/popododo0720/vws-network/internal/network"
)

// TestImportFlags exits 2 on a missing or conflicting flag before anything
// opens, and picks the mode otherwise.
func TestImportFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-store-namespace", "vws-system"}, "-from is required"},
		{[]string{"-from", "net.db"}, "-store-namespace is required"},
		{[]string{"-from", "net.db", "-store-namespace", "ns", "-dry-run", "-verify"}, "cannot both be set"},
		{[]string{"-from", "net.db", "-store-namespace", "ns", "extra"}, "unexpected arguments"},
		{[]string{"-db", "x"}, "flag provided but not defined"},
	} {
		var stdout, stderr bytes.Buffer
		if code := importMain(tc.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("%v: code %d stderr %q, want 2 and %q", tc.args, code, stderr.String(), tc.want)
		}
	}
	for _, tc := range []struct {
		dryRun, verify bool
		want           network.ImportMode
	}{
		{false, false, network.ImportWrite},
		{true, false, network.ImportDryRun},
		{false, true, network.ImportVerify},
	} {
		got, err := parseImport("net.db", "ns", tc.dryRun, tc.verify, 0)
		if err != nil || got != tc.want {
			t.Fatalf("dry-run %v verify %v: %v %v", tc.dryRun, tc.verify, got, err)
		}
	}
}
