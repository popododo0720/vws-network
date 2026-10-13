//go:build envtest

package network

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestEnvtestOpenCreatesDirectNetworks starts with a flat external network
// and a tagged direct network on the same NIC, and checks that both share
// one ProviderNetwork, that the direct Subnet is tied to its NAD and that a
// restart accepts what the first start made.
func TestEnvtestOpenCreatesDirectNetworks(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	cfg := Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("10.30.0.128/25"),
		Workers:      1,
		KubeOVN: KubeOVNConfig{
			Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "external", NADNamespace: envNamespace,
			Underlay: flatUnderlay(), Direct: []DirectNetwork{prodDirect()},
		},
	}
	for range 2 {
		svc, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		closeOrFail(t, svc.Close)
	}
	if got := nestedString(get(t, c, gvkProviderNetwork, "ext").Object, "spec", "defaultInterface"); got != "ens3" {
		t.Fatalf("provider network interface %q", got)
	}
	if got := nestedString(get(t, c, gvkVlan, "ext-vlan200").Object, "spec", "provider"); got != "ext" {
		t.Fatalf("direct vlan provider %q", got)
	}
	sub := get(t, c, gvkSubnet, "vws-direct-prod").Object
	wantProvider := "vws-direct-prod." + envNamespace + ".ovn"
	if nestedString(sub, "spec", "vlan") != "ext-vlan200" || nestedString(sub, "spec", "provider") != wantProvider || nestedString(sub, "spec", "cidrBlock") != "192.168.200.0/24" {
		t.Fatalf("direct subnet spec %v", sub["spec"])
	}
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(gvkNAD)
	if err := c.Get(ctx, client.ObjectKey{Namespace: envNamespace, Name: "vws-direct-prod"}, nad); err != nil {
		t.Fatal(err)
	}
	if conf := nestedString(nad.Object, "spec", "config"); !strings.Contains(conf, wantProvider) {
		t.Fatalf("nad config %s", conf)
	}
	cfg.KubeOVN.Direct[0].CIDR = mustP("192.168.201.0/24")
	cfg.KubeOVN.Direct[0].Gateway = netip.MustParseAddr("192.168.201.1")
	cfg.KubeOVN.Direct[0].Exclude = nil
	cfg.DatabasePath = filepath.Join(t.TempDir(), "other.db")
	if _, err := Open(ctx, cfg); !isConflict(err) {
		t.Fatalf("changed direct cidr err %v, want Conflict", err)
	}
}
