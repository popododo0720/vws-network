//go:build envtest

package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/popododo0720/vws-network/internal/domain"
)

const envNamespace = "vws-system"

// startEnv runs a real kube-apiserver and etcd with the CRDs in testdata.
// It skips when the envtest binaries are not installed.
func startEnv(t *testing.T) (*envtest.Environment, client.Client) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; install with setup-envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNamespace}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return env, c
}

func envKube(c client.Client, snat bool) *kubeOVN {
	return &kubeOVN{c: c, externalSubnet: "external", nadNamespace: envNamespace, snat: snat}
}

func TestEnvtestSubnetAndSNATLifecycle(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := envKube(c, true)
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	for range 2 {
		if err := k.ensureVPC(ctx, vpc); err != nil {
			t.Fatal(err)
		}
		if err := k.ensureSubnet(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	gw := getObj(t, c, gvkNatGW, "nat-vpc-1")
	if lan, _, _ := unstructured.NestedString(gw.Object, "spec", "lanIp"); lan != "10.0.1.254" {
		t.Fatalf("lanIp %q", lan)
	}
	getObj(t, c, gvkSnat, "snat-subnet-1")
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(gvkNAD)
	if err := c.Get(ctx, client.ObjectKey{Namespace: envNamespace, Name: "subnet-1"}, nad); err != nil {
		t.Fatalf("nad: %v", err)
	}
	if nad.GetAnnotations()[annotationID] != "subnet-1" {
		t.Fatalf("nad annotations %v", nad.GetAnnotations())
	}
	for range 2 {
		if err := k.deleteSubnet(ctx, sub); err != nil {
			t.Fatal(err)
		}
		if err := k.deleteVPC(ctx, vpc); err != nil {
			t.Fatal(err)
		}
	}
	for _, gvk := range []struct {
		kind, name string
	}{{"rule", "snat-subnet-1"}, {"eip", "snat-subnet-1"}, {"gw", "nat-vpc-1"}} {
		absent(t, c, gvk.name, gvk.kind)
	}
	s := &unstructured.Unstructured{}
	s.SetGroupVersionKind(gvkSubnet)
	if err := c.Get(ctx, client.ObjectKey{Name: "subnet-1"}, s); !apierrors.IsNotFound(err) {
		t.Fatalf("subnet still present: %v", err)
	}
}

func TestEnvtestRulesAndElasticIP(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := envKube(c, false)
	if err := k.ensureGroup(ctx, domain.SecurityGroup{ID: "sg-1"}); err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	for range 2 {
		if err := k.ensureRule(ctx, "sg-1", rule); err != nil {
			t.Fatal(err)
		}
	}
	g := getObj(t, c, gvkGroup, "sg-1")
	rules, _, _ := unstructured.NestedSlice(g.Object, "spec", "ingressRules")
	if len(rules) != 1 {
		t.Fatalf("ingress rules %v", rules)
	}
	first, _ := rules[0].(map[string]any)
	if first["portRangeMin"] != int64(22) || first["remoteAddress"] != "0.0.0.0/0" {
		t.Fatalf("stored rule %v", first)
	}
	if err := k.deleteRule(ctx, "sg-1", rule); err != nil {
		t.Fatal(err)
	}
	g = getObj(t, c, gvkGroup, "sg-1")
	if rules, _, _ := unstructured.NestedSlice(g.Object, "spec", "ingressRules"); len(rules) != 0 {
		t.Fatalf("rule survived delete: %v", rules)
	}

	e, private := eipFixture()
	addr := mustP(private + "/32").Addr()
	if err := k.ensureElasticIP(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureNAT(ctx, e, addr); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteElasticIP(ctx, e); err == nil {
		t.Fatal("released an elastic ip that is still bound")
	}
	if err := k.deleteNAT(ctx, e, addr); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteElasticIP(ctx, e); err != nil {
		t.Fatal(err)
	}
	absent(t, c, "eip-1", "eip")
}

func TestEnvtestRejectsInvalidName(t *testing.T) {
	_, c := startEnv(t)
	err := envKube(c, false).ensureVPC(context.Background(), domain.VPC{ID: "VPC_Upper", CIDR: mustP("10.0.0.0/16")})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("err %v, want an API Invalid", err)
	}
}

func kubeconfigFor(t *testing.T, env *envtest.Environment) string {
	t.Helper()
	u, err := env.AddUser(envtest.User{Name: "vws", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := u.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvtestServiceThroughOpen(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	svc, err := Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("198.51.100.0/24"),
		Workers:      1,
		KubeOVN:      KubeOVNConfig{Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "external", NADNamespace: envNamespace, SNAT: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(op.Resource)
	subOp, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpc, CIDR: mustP("10.8.1.0/24"), Zone: "az1"})
	if err != nil {
		t.Fatal(err)
	}
	sub := domain.SubnetID(subOp.Resource)
	waitFor(t, func() bool {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gvkSnat)
		return c.Get(ctx, client.ObjectKey{Name: snatName(sub)}, o) == nil
	})
	if _, err := svc.CreateSubnet(ctx, call("proj", "bad"), domain.CreateSubnet{VPC: vpc, CIDR: mustP("10.9.1.0/24"), Zone: "az1"}); kind(err) != domain.Invalid {
		t.Fatalf("subnet outside vpc err = %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
