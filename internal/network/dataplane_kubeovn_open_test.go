package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
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
		"nad namespace only":          {NADNamespace: "vws-system"},
		"snat only":                   {SNAT: true},
		"in-cluster without external": {NADNamespace: "n", SNAT: true},
		"snat without external":       {Kubeconfig: writeKubeconfig(t), NADNamespace: "n", SNAT: true},
		"missing external subnet":     {Kubeconfig: writeKubeconfig(t), NADNamespace: "n"},
	} {
		if _, err := newDataplane(cfg); !isInvalid(err) {
			t.Fatalf("%s: err %v, want Invalid", name, err)
		}
	}
	if _, err := newDataplane(KubeOVNConfig{Kubeconfig: filepath.Join(t.TempDir(), "absent"), ExternalSubnet: "ext", NADNamespace: "n"}); err == nil {
		t.Fatal("absent kubeconfig accepted")
	}
}

func TestOpenSelectsDataplaneFromConfig(t *testing.T) {
	ctx := context.Background()
	pool := mustP("198.51.100.0/24")
	svc, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "a.db"), AddressPool: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.dp.(measured).dataplane.(*fakeDataplane); !ok {
		t.Fatalf("no kubeconfig gave %T", svc.dp)
	}
	closeOrFail(t, svc.Close)
	// A kubeconfig selects Kube-OVN, whose start reads the external Subnet
	// (#52); with no API server behind it, Open fails instead of starting.
	if _, err := Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "b.db"),
		AddressPool:  pool,
		KubeOVN:      KubeOVNConfig{Kubeconfig: writeKubeconfig(t), ExternalSubnet: "ext", NADNamespace: "vws-system"},
	}); err == nil || isInvalid(err) || !strings.Contains(err.Error(), "get external subnet ext") {
		t.Fatalf("unreachable kube-ovn err %v", err)
	}
	if _, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "c.db"), AddressPool: pool, KubeOVN: KubeOVNConfig{ExternalSubnet: "x"}}); !errors.Is(err, rest.ErrNotInCluster) {
		t.Fatalf("external subnet without kubeconfig err %v, want ErrNotInCluster", err)
	}
	if _, err := Open(ctx, Config{DatabasePath: filepath.Join(t.TempDir(), "d.db"), AddressPool: pool, KubeOVN: KubeOVNConfig{Kubeconfig: writeKubeconfig(t), NADNamespace: "vws-system"}}); !isInvalid(err) {
		t.Fatalf("kubeconfig without external subnet err %v", err)
	}
}

func stubInCluster(t *testing.T, host string, err error) {
	t.Helper()
	old := inClusterConfig
	inClusterConfig = func() (*rest.Config, error) {
		if err != nil {
			return nil, err
		}
		return &rest.Config{Host: host}, nil
	}
	t.Cleanup(func() { inClusterConfig = old })
}

func TestNewDataplaneInClusterWithoutKubeconfig(t *testing.T) {
	stubInCluster(t, "https://127.0.0.1:1", nil)
	dp, err := newDataplane(KubeOVNConfig{ExternalSubnet: "ext", NADNamespace: "vws-system", SNAT: true})
	if err != nil {
		t.Fatal(err)
	}
	k, ok := dp.(*kubeOVN)
	if !ok {
		t.Fatalf("in-cluster gave %T, want *kubeOVN", dp)
	}
	if k.externalSubnet != "ext" || k.nadNamespace != "vws-system" || !k.snat {
		t.Fatalf("kube config %q %q %v", k.externalSubnet, k.nadNamespace, k.snat)
	}
}

func TestNewDataplaneKubeconfigBeatsInCluster(t *testing.T) {
	stubInCluster(t, "", errors.New("in-cluster must not be read"))
	dp, err := newDataplane(KubeOVNConfig{Kubeconfig: writeKubeconfig(t), ExternalSubnet: "ext", NADNamespace: "vws-system"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dp.(*kubeOVN); !ok {
		t.Fatalf("kubeconfig gave %T", dp)
	}
}

func TestNewDataplaneNotInCluster(t *testing.T) {
	stubInCluster(t, "", rest.ErrNotInCluster)
	_, err := newDataplane(KubeOVNConfig{ExternalSubnet: "ext", NADNamespace: "vws-system"})
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Fatalf("err %v, want ErrNotInCluster", err)
	}
	if !strings.Contains(err.Error(), "kube-ovn needs -kubeconfig or an in-cluster service account") {
		t.Fatalf("err %q lacks the remedy", err)
	}
	if _, err := newDataplane(KubeOVNConfig{}); err != nil {
		t.Fatalf("fake dataplane read the in-cluster config: %v", err)
	}
}

func TestInClusterErrorsNameTheMissingOption(t *testing.T) {
	stubInCluster(t, "https://127.0.0.1:1", nil)
	for want, cfg := range map[string]KubeOVNConfig{
		"external subnet is required": {NADNamespace: "n"},
	} {
		_, err := newDataplane(cfg)
		if !isInvalid(err) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err %v, want Invalid containing %q", err, want)
		}
	}
}

func TestNADNamespaceDefaultsWhenSelected(t *testing.T) {
	stubInCluster(t, "https://127.0.0.1:1", nil)
	dp, err := newDataplane(KubeOVNConfig{ExternalSubnet: "ext"})
	if err != nil {
		t.Fatal(err)
	}
	k, ok := dp.(*kubeOVN)
	if !ok {
		t.Fatalf("got %T, want *kubeOVN", dp)
	}
	if k.nadNamespace != "vws-system" {
		t.Fatalf("nad namespace %q, want vws-system", k.nadNamespace)
	}
}
