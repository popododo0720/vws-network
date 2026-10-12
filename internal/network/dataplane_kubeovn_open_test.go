package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const kubeconfigYAML = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: x
  context:
    cluster: c
    user: u
current-context: x
users:
- name: u
  user:
    token: t
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(p, []byte(kubeconfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewDataplaneSelection(t *testing.T) {
	dp, err := newDataplane(KubeOVNConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dp.(*fakeDataplane); !ok {
		t.Fatalf("zero config gave %T, want *fakeDataplane", dp)
	}
	dp, err = newDataplane(KubeOVNConfig{Kubeconfig: writeKubeconfig(t), ExternalSubnet: "ext", NADNamespace: "vws-system"})
	if err != nil {
		t.Fatal(err)
	}
	k, ok := dp.(*kubeOVN)
	if !ok {
		t.Fatalf("kubeconfig gave %T, want *kubeOVN", dp)
	}
	if k.externalSubnet != "ext" || k.nadNamespace != "vws-system" {
		t.Fatalf("kube config %q %q", k.externalSubnet, k.nadNamespace)
	}
}

func TestNewDataplaneRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]KubeOVNConfig{
		"options without kubeconfig": {ExternalSubnet: "ext"},
		"nad namespace only":         {NADNamespace: "vws-system"},
		"snat without kubeconfig":    {SNAT: true},
		"snat without external":      {Kubeconfig: writeKubeconfig(t), NADNamespace: "n", SNAT: true},
		"missing external subnet":    {Kubeconfig: writeKubeconfig(t), NADNamespace: "n"},
		"missing nad namespace":      {Kubeconfig: writeKubeconfig(t), ExternalSubnet: "ext"},
	} {
		if _, err := newDataplane(cfg); !isInvalid(err) {
			t.Fatalf("%s: err %v, want Invalid", name, err)
		}
	}
	if _, err := newDataplane(KubeOVNConfig{Kubeconfig: filepath.Join(t.TempDir(), "absent"), ExternalSubnet: "ext", NADNamespace: "n"}); err == nil {
		t.Fatal("absent kubeconfig accepted")
	}
}

func TestOpenUsesKubeOVNOnlyWithKubeconfig(t *testing.T) {
	ctx := context.Background()
	pool := mustP("198.51.100.0/24")
	svc, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "a.db"), AddressPool: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.dp.(*fakeDataplane); !ok {
		t.Fatalf("no kubeconfig gave %T", svc.dp)
	}
	_ = svc.Close()
	svc, err = Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "b.db"),
		AddressPool:  pool,
		KubeOVN:      KubeOVNConfig{Kubeconfig: writeKubeconfig(t), ExternalSubnet: "ext", NADNamespace: "vws-system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.dp.(*kubeOVN); !ok {
		t.Fatalf("kubeconfig gave %T", svc.dp)
	}
	_ = svc.Close()
	if _, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "c.db"), AddressPool: pool, KubeOVN: KubeOVNConfig{ExternalSubnet: "x"}}); !isInvalid(err) {
		t.Fatalf("stray option err %v", err)
	}
	if _, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "d.db"), AddressPool: pool, KubeOVN: KubeOVNConfig{Kubeconfig: writeKubeconfig(t), NADNamespace: "vws-system"}}); !isInvalid(err) {
		t.Fatalf("kubeconfig without external subnet err %v", err)
	}
}
