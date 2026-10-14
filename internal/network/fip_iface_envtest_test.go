//go:build envtest

package network

import (
	"context"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestNic0UsesSinglePodInterface is vws-test 77h: console-api asks for
// device nic0, the VMI reports only the KubeVirt pod network "default" with
// no interfaceName. The association uses that interface and creates the OvnFip.
func TestEnvtestNic0UsesSinglePodInterface(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	svc, err := Open(ctx, Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  mustP("10.78.0.128/25"),
		Workers:      1,
		KubeOVN: KubeOVNConfig{
			Kubeconfig: kubeconfigFor(t, env), ExternalSubnet: "external", NADNamespace: envNamespace,
			VMNamespace: envNamespace,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("172.31.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(vpcOp.Resource)
	if _, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpc, CIDR: mustP("172.31.76.0/24"), Zone: "z"}); err != nil {
		t.Fatal(err)
	}
	eipOp, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: vpc})
	if err != nil {
		t.Fatal(err)
	}
	id := domain.ElasticIPID(eipOp.Resource)
	waitFor(t, func() bool {
		e, err := svc.GetElasticIP(ctx, "proj", id)
		return err == nil && e.Status == domain.StatusAvailable
	})
	vmi := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{},
		"status": map[string]any{"interfaces": []any{map[string]any{
			"infoSource": "domain", "ipAddress": "172.31.76.3", "name": "default", "podInterfaceName": "eth0",
		}}},
	}}
	vmi.SetGroupVersionKind(gvkVMI)
	vmi.SetNamespace(envNamespace)
	vmi.SetName("i-0123456789abcdef0")
	if err := c.Create(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	assoc, err := svc.UpdateElasticIP(ctx, call("proj", "assoc"), domain.UpdateElasticIP{
		ElasticIP: id, SetAssociation: true, Instance: "projects/proj/zones/z/instances/i-0123456789abcdef0", Device: "nic0",
	})
	if err != nil {
		t.Fatal(err)
	}
	fipName := fipName(id, mustP("172.31.76.3/32").Addr())
	waitFor(t, func() bool {
		f := &unstructured.Unstructured{}
		f.SetGroupVersionKind(gvkFip)
		if err := c.Get(ctx, client.ObjectKey{Name: fipName}, f); err != nil {
			return false
		}
		return nestedString(f.Object, "spec", "v4Ip") == "172.31.76.3" && nestedString(f.Object, "spec", "ovnEip") == string(id)
	})
	waitFor(t, func() bool {
		op, err := svc.GetOperation(ctx, "proj", assoc.ID)
		return err == nil && op.State == domain.OpSucceeded
	})
}
