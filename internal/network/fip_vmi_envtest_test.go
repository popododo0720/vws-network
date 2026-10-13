//go:build envtest

package network

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestAttachedAddressGetsFipFromVMI is UpdateAddress spec.association
// end to end through a real API server: while the VMI has no IP the address
// waits (no OvnFip, operation still pending, never failed); once KubeVirt
// reports the interface IP the OvnFip appears with it; clearing the
// association deletes the OvnFip.
func TestEnvtestAttachedAddressGetsFipFromVMI(t *testing.T) {
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
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(vpcOp.Resource)
	if _, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpc, CIDR: mustP("10.8.1.0/24"), Zone: "z"}); err != nil {
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

	// The VMI exists but KubeVirt has not reported an IP yet.
	vmi := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{},
		"status": map[string]any{"interfaces": []any{map[string]any{"name": "default", "interfaceName": "eth0"}}},
	}}
	vmi.SetGroupVersionKind(gvkVMI)
	vmi.SetNamespace(envNamespace)
	vmi.SetName("i-0123456789abcdef0")
	if err := c.Create(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	instance := "projects/proj/zones/z/instances/i-0123456789abcdef0"
	assoc, err := svc.UpdateElasticIP(ctx, call("proj", "assoc"), domain.UpdateElasticIP{ElasticIP: id, SetAssociation: true, Instance: instance, Device: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	fipName := fipName(id, mustP("10.8.1.23/32").Addr())
	fips := func() []unstructured.Unstructured {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(gvkFip.GroupVersion().WithKind("OvnFipList"))
		if err := c.List(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	time.Sleep(2 * time.Second)
	if n := len(fips()); n != 0 {
		t.Fatalf("%d OvnFip before the VMI has an IP", n)
	}
	if op, err := svc.GetOperation(ctx, "proj", assoc.ID); err != nil || op.State == domain.OpFailed || op.State == domain.OpSucceeded {
		t.Fatalf("association while waiting for the IP: %+v %v, want still pending", op, err)
	}

	// KubeVirt reports the guest interface address.
	if err := c.Get(ctx, client.ObjectKeyFromObject(vmi), vmi); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"name": "default", "interfaceName": "eth0", "ipAddress": "10.8.1.23"}}, "status", "interfaces"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		f := &unstructured.Unstructured{}
		f.SetGroupVersionKind(gvkFip)
		if err := c.Get(ctx, client.ObjectKey{Name: fipName}, f); err != nil {
			return false
		}
		return nestedString(f.Object, "spec", "v4Ip") == "10.8.1.23" && nestedString(f.Object, "spec", "ovnEip") == string(id)
	})
	waitFor(t, func() bool {
		op, err := svc.GetOperation(ctx, "proj", assoc.ID)
		return err == nil && op.State == domain.OpSucceeded
	})
	if e, err := svc.GetElasticIP(ctx, "proj", id); err != nil || e.Private != mustP("10.8.1.23/32").Addr() || e.Subnet == "" {
		t.Fatalf("stored address %+v %v", e, err)
	}

	// Clearing the association deletes the OvnFip.
	if _, err := svc.UpdateElasticIP(ctx, call("proj", "dis"), domain.UpdateElasticIP{ElasticIP: id, SetAssociation: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		f := &unstructured.Unstructured{}
		f.SetGroupVersionKind(gvkFip)
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Name: fipName}, f))
	})
	if e, err := svc.GetElasticIP(ctx, "proj", id); err != nil || e.Private.IsValid() || e.Instance != "" {
		t.Fatalf("after disassociation %+v %v", e, err)
	}
}
