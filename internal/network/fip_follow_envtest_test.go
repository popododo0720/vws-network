//go:build envtest

package network

import (
	"context"
	"net/netip"
	"path/filepath"
	"sort"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/popododo0720/vws-network/internal/domain"
)

func vmiWithIP(name, ip string) *unstructured.Unstructured {
	iface := map[string]any{"name": "default", "interfaceName": "eth0"}
	if ip != "" {
		iface["ipAddress"] = ip
	}
	vmi := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}, "status": map[string]any{"interfaces": []any{iface}}}}
	vmi.SetGroupVersionKind(gvkVMI)
	vmi.SetNamespace(envNamespace)
	vmi.SetName(name)
	return vmi
}

func setVMIIP(t *testing.T, c client.Client, name, ip string) {
	t.Helper()
	ctx := context.Background()
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(gvkVMI)
	if err := c.Get(ctx, client.ObjectKey{Namespace: envNamespace, Name: name}, vmi); err != nil {
		t.Fatal(err)
	}
	vmi.Object["status"] = vmiWithIP(name, ip).Object["status"]
	if err := c.Update(ctx, vmi); err != nil {
		t.Fatal(err)
	}
}

// fipIPs is the spec.v4Ip of every OvnFip, sorted.
func fipIPs(t *testing.T, c client.Client) []string {
	t.Helper()
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(gvkFip.GroupVersion().WithKind("OvnFipList"))
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range l.Items {
		out = append(out, nestedString(f.Object, "spec", "v4Ip"))
	}
	sort.Strings(out)
	return out
}

// TestEnvtestFipFollowsVMIIP is #86 through a real API server: the OvnFip and
// the stored private address follow the attached VMI's new IP; an IP that is
// briefly empty (VM restarting) changes nothing; a VMI no address is attached
// to changes nothing.
func TestEnvtestFipFollowsVMIIP(t *testing.T) {
	env, c := startEnv(t)
	ctx := context.Background()
	for _, v := range []*unstructured.Unstructured{vmiWithIP("i-a", "10.8.1.23"), vmiWithIP("i-b", "10.8.1.40")} {
		if err := c.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
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
	private := func() netip.Addr {
		e, err := svc.GetElasticIP(ctx, "proj", id)
		if err != nil {
			t.Fatal(err)
		}
		return e.Private
	}
	waitFor(t, func() bool {
		e, err := svc.GetElasticIP(ctx, "proj", id)
		return err == nil && e.Status == domain.StatusAvailable
	})
	if _, err := svc.UpdateElasticIP(ctx, call("proj", "assoc"), domain.UpdateElasticIP{ElasticIP: id, SetAssociation: true, Instance: "projects/proj/zones/z/instances/i-a", Device: "eth0"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { f := fipIPs(t, c); return len(f) == 1 && f[0] == "10.8.1.23" })

	// The VM restarts: its interface briefly has no IP. Nothing moves.
	setVMIIP(t, c, "i-a", "")
	time.Sleep(2 * time.Second)
	if f := fipIPs(t, c); len(f) != 1 || f[0] != "10.8.1.23" || private() != mustP("10.8.1.23/32").Addr() {
		t.Fatalf("empty IP changed the address: fips %v private %v", f, private())
	}

	// It comes back with a new IP: the OvnFip and the stored address follow.
	setVMIIP(t, c, "i-a", "10.8.1.24")
	waitFor(t, func() bool { f := fipIPs(t, c); return len(f) == 1 && f[0] == "10.8.1.24" })
	waitFor(t, func() bool { return private() == mustP("10.8.1.24/32").Addr() })

	// A VMI no address is attached to changes: nothing changes.
	setVMIIP(t, c, "i-b", "10.8.1.41")
	time.Sleep(2 * time.Second)
	if f := fipIPs(t, c); len(f) != 1 || f[0] != "10.8.1.24" || private() != mustP("10.8.1.24/32").Addr() {
		t.Fatalf("an unattached VMI changed the address: fips %v private %v", f, private())
	}
}

// TestEnvtestVMIWatchNeedsOnlyTheVMIRole: the follow loop's list and watch
// work for a user that holds only vmiRules in the VM namespace.
func TestEnvtestVMIWatchNeedsOnlyTheVMIRole(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	subj := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "vmi-watcher", APIGroup: rbacv1.GroupName}}
	for _, o := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "vmi-watcher", Namespace: envNamespace}, Rules: vmiRules},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "vmi-watcher", Namespace: envNamespace}, Subjects: subj, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "vmi-watcher"}},
	} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	u, err := env.AddUser(envtest.User{Name: "vmi-watcher"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := client.NewWithWatch(u.Config(), client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	k := &kubeOVN{c: w, w: w, vmNamespace: envNamespace}
	svc, err := newService(ctx, newMemRepo(), newFake(), mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := svc.vmiWatchOnce(wctx, k); err != nil {
		t.Fatalf("list and watch with only the VMI Role: %v", err)
	}
}
