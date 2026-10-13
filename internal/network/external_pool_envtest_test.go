//go:build envtest

package network

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEnvtestPoolMustSitInOperatorExternalSubnet starts without underlay flags
// against an external Subnet the operator made and checks #52: a missing
// Subnet or a pool outside its cidrBlock stops the start as Invalid.
func TestEnvtestPoolMustSitInOperatorExternalSubnet(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	kc := kubeconfigFor(t, env)
	open := func(pool string) error {
		svc, err := Open(ctx, Config{
			DatabasePath: filepath.Join(t.TempDir(), "net.db"),
			AddressPool:  mustP(pool),
			Workers:      1,
			KubeOVN:      KubeOVNConfig{Kubeconfig: kc, ExternalSubnet: "operator-ext", NADNamespace: envNamespace},
		})
		if err == nil {
			closeOrFail(t, svc.Close)
		}
		return err
	}
	if err := open("10.30.0.128/25"); !isInvalid(err) {
		t.Fatalf("missing external subnet err %v, want Invalid", err)
	}
	seed(t, c, gvkSubnet, "", "operator-ext", "", map[string]any{"protocol": "IPv4", "cidrBlock": "10.30.0.0/24"}, nil)
	if err := open("10.30.0.128/25"); err != nil {
		t.Fatalf("pool inside: %v", err)
	}
	for _, pool := range []string{"10.31.0.0/24", "10.30.0.0/23"} {
		if err := open(pool); !isInvalid(err) {
			t.Fatalf("pool %s err %v, want Invalid", pool, err)
		}
	}
}
