//go:build envtest

package network

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEnvtestOpenCreatesUnderlay starts the service against a real API server
// with a tagged underlay and checks that the external network exists and
// that a restart accepts what the first start made.
func TestEnvtestOpenCreatesUnderlay(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	u := flatUnderlay()
	u.VLANID = 204
	cfg := Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("10.30.0.128/25"),
		Workers:      1,
		KubeOVN:      KubeOVNConfig{Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "vws-external", NADNamespace: envNamespace, Underlay: u},
	}
	for range 2 {
		svc, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Ready(ctx); err != nil {
			t.Fatalf("ready: %v", err)
		}
		closeOrFail(t, svc.Close)
	}
	if got := nestedString(get(t, c, gvkSubnet, "vws-external").Object, "spec", "vlan"); got != "ext-vlan204" {
		t.Fatalf("external subnet vlan %q", got)
	}
	cfg.KubeOVN.Underlay.Interface = "ens4"
	cfg.DatabasePath = filepath.Join(t.TempDir(), "other.db")
	if _, err := Open(ctx, cfg); !isConflict(err) {
		t.Fatalf("changed interface err %v, want Conflict", err)
	}
}
